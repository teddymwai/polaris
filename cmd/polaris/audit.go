// Copyright 2020 FairwindsOps Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"

	workloads "github.com/fairwindsops/insights-plugins/plugins/workloads"
	workloadsPkg "github.com/fairwindsops/insights-plugins/plugins/workloads/pkg"

	"github.com/fairwindsops/polaris/pkg/auth"
	cfg "github.com/fairwindsops/polaris/pkg/config"
	"github.com/fairwindsops/polaris/pkg/insights"
	"github.com/fairwindsops/polaris/pkg/kube"
	"github.com/fairwindsops/polaris/pkg/validator"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

var (
	setExitCode         bool
	onlyShowFailedTests bool
	minScore            int
	auditOutputURL      string
	auditOutputFile     string
	auditOutputFormat   string
	resourceToAudit     string
	useColor            bool
	helmChart           string
	helmValues          string
	checks              []string
	auditNamespace      string
	skipSslValidation   bool
	uploadInsights      bool
	clusterName         string
)

func init() {
	rootCmd.AddCommand(auditCmd)
	auditCmd.PersistentFlags().StringVar(&auditPath, "audit-path", "", "If specified, audits one or more YAML files instead of a cluster.")
	auditCmd.PersistentFlags().BoolVar(&setExitCode, "set-exit-code-on-danger", false, "Set an exit code of 3 when the audit contains danger-level issues.")
	auditCmd.PersistentFlags().BoolVar(&onlyShowFailedTests, "only-show-failed-tests", false, "If specified, audit output will only show failed tests.")
	auditCmd.PersistentFlags().IntVar(&minScore, "set-exit-code-below-score", 0, "Set an exit code of 4 when the score is below this threshold (1-100).")
	auditCmd.PersistentFlags().StringVar(&auditOutputURL, "output-url", "", "Destination URL to send audit results.")
	auditCmd.PersistentFlags().StringVar(&auditOutputFile, "output-file", "", "Destination file for audit results.")
	auditCmd.PersistentFlags().StringVarP(&auditOutputFormat, "format", "f", "json", "Output format for results - json, yaml, pretty, or score.")
	auditCmd.PersistentFlags().BoolVar(&useColor, "color", true, "Whether to use color in pretty format.")
	auditCmd.PersistentFlags().StringVar(&displayName, "display-name", "", "An optional identifier for the audit.")
	auditCmd.PersistentFlags().StringVar(&resourceToAudit, "resource", "", "Audit a specific resource, in the format namespace/kind/version/name, e.g. nginx-ingress/Deployment.apps/v1/default-backend.")
	auditCmd.PersistentFlags().StringVar(&helmChart, "helm-chart", "", "Will fill out Helm template")
	auditCmd.PersistentFlags().StringVar(&helmValues, "helm-values", "", "Optional flag to add helm values")
	auditCmd.PersistentFlags().StringSliceVar(&checks, "checks", []string{}, "Optional flag to specify specific checks to check")
	auditCmd.PersistentFlags().StringVar(&auditNamespace, "namespace", "", "Namespace to audit. Only applies to in-cluster audits")
	auditCmd.PersistentFlags().BoolVar(&skipSslValidation, "skip-ssl-validation", false, "Skip https certificate verification")
	auditCmd.PersistentFlags().BoolVar(&uploadInsights, "upload-insights", false, "Upload scan results to Fairwinds Insights")
	auditCmd.PersistentFlags().StringVar(&clusterName, "cluster-name", "", "Set --cluster-name to a descriptive name for the cluster you're auditing")
}

var auditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Runs a one-time audit.",
	Long:  `Runs a one-time audit.`,
	Run: func(cmd *cobra.Command, args []string) {
		if displayName != "" {
			config.DisplayName = displayName
		}
		if len(checks) > 0 {
			targetChecks := make(map[string]bool)
			for _, check := range checks {
				targetChecks[check] = true
			}
			for key := range config.Checks {
				if isTarget := targetChecks[key]; !isTarget {
					config.Checks[key] = cfg.SeverityIgnore
				}
			}
		}
		if auditNamespace != "" {
			if helmChart != "" {
				logrus.Warn("--namespace and --helm-chart are mutually exclusive. --namespace will be ignored.")
			}
			if auditPath != "" {
				logrus.Warn("--namespace and --audit-path are mutually exclusive. --namespace will be ignored.")
			}
			config.Namespace = auditNamespace
		}
		if helmChart != "" {
			var err error
			auditPath, err = ProcessHelmTemplates(helmChart, helmValues)
			if err != nil {
				logrus.Errorf("Couldn't process helm chart: %v", err)
				os.Exit(1)
			}
		}
		if uploadInsights && len(clusterName) == 0 {
			logrus.Error("cluster-name is required when using --upload-insights")
			os.Exit(1)
		}
		if uploadInsights {
			if auditPath != "" {
				logrus.Errorf("upload-insights and audit-path are not supported when used simultaneously")
				os.Exit(1)
			}
			if !auth.IsLoggedIn() {
				err := auth.HandleLogin(insightsHost)
				if err != nil {
					logrus.Errorf("error handling logging: %v", err)
					os.Exit(1)
				}
			}
		}

		ctx := context.TODO()
		k, err := kube.CreateResourceProvider(ctx, auditPath, resourceToAudit, config)
		if err != nil {
			logrus.Errorf("Error fetching Kubernetes resources %v", err)
			os.Exit(1)
		}

		auditData, err := validator.RunAudit(config, k)
		if err != nil {
			logrus.Errorf("Error while running audit on resources: %v", err)
			os.Exit(1)
		}

		if uploadInsights {
			auth, err := auth.GetAuth(insightsHost)
			if err != nil {
				logrus.Errorf("getting auth: %v", err)
				os.Exit(1)
			}
			// fetch workloads using workload plugin... or should we adapt the workloads from above?
			dynamicClient, restMapper, clientSet, host, err := kube.GetKubeClient(ctx, "")
			if err != nil {
				logrus.Errorf("getting the kubernetes client: %v", err)
				os.Exit(1)
			}
			k8sResources, err := workloadsPkg.CreateResourceProviderFromAPI(ctx, dynamicClient, restMapper, clientSet, host)
			if err != nil {
				logrus.Errorf("creating resource provider: %v", err)
				os.Exit(1)
			}

			insightsClient := insights.NewHTTPClient(insightsHost, auth.Organization, auth.Token)
			insightsReporter := insights.NewInsightsReporter(insightsClient)
			wr := insights.WorkloadsReport{Version: workloads.Version, Payload: *k8sResources}
			pr := insights.PolarisReport{Version: version, Payload: auditData}
			logrus.Infof("Uploading to Fairwinds Insights organization '%s/%s'...", auth.Organization, clusterName)
			err = insightsReporter.ReportAuditToFairwindsInsights(clusterName, wr, pr)
			if err != nil {
				logrus.Errorf("reporting audit file to insights: %v", err)
				os.Exit(1)
			}
			logrus.Println("Success! You can see your results at:")
			logrus.Printf("%s/orgs/%s/clusters/%s/action-items\n", insightsHost, auth.Organization, clusterName)
		} else {
			outputAudit(auditData, auditOutputFile, auditOutputURL, auditOutputFormat, useColor, onlyShowFailedTests)
		}

		summary := auditData.GetSummary()
		score := summary.GetScore()
		if setExitCode && summary.Dangers > 0 {
			logrus.Infof("%d danger items found in audit", summary.Dangers)
			os.Exit(3)
		} else if minScore != 0 && score < uint(minScore) {
			logrus.Infof("Audit score of %d is less than the provided minimum of %d", score, minScore)
			os.Exit(4)
		}
	},
}

// ProcessHelmTemplates turns helm into yaml to be processed by Polaris or the other tools.
func ProcessHelmTemplates(helmChart, helmValues string) (string, error) {
	cmd := exec.Command("helm", "dependency", "update", helmChart)
	output, err := cmd.CombinedOutput()
	if err != nil {
		logrus.Error(string(output))
		return "", err
	}

	dir, err := os.MkdirTemp("", "*")
	if err != nil {
		return "", err
	}
	params := []string{
		"template", helmChart,
		"--generate-name",
		"--output-dir",
		dir,
	}
	if helmValues != "" {
		params = append(params, "--values", helmValues)
	}

	cmd = exec.Command("helm", params...)
	output, err = cmd.CombinedOutput()

	if err != nil {
		logrus.Error(string(output))
		return "", err
	}
	return dir, nil
}

func outputAudit(auditData validator.AuditData, outputFile, outputURL, outputFormat string, useColor bool, onlyShowFailedTests bool) {
	if onlyShowFailedTests {
		auditData = auditData.RemoveSuccessfulResults()
	}
	var outputBytes []byte
	var err error
	if outputFormat == "score" {
		outputBytes = []byte(fmt.Sprintf("%d\n", auditData.GetSummary().GetScore()))
	} else if outputFormat == "yaml" {
		var jsonBytes []byte
		jsonBytes, err = json.Marshal(auditData)
		if err == nil {
			outputBytes, err = yaml.JSONToYAML(jsonBytes)
		}
	} else if outputFormat == "pretty" {
		outputBytes = []byte(auditData.GetPrettyOutput(useColor))
	} else {
		outputBytes, err = json.MarshalIndent(auditData, "", "  ")
	}
	if err != nil {
		logrus.Errorf("Error marshalling audit: %v", err)
		os.Exit(1)
	}
	if outputURL == "" && outputFile == "" {
		os.Stdout.Write(outputBytes)
	} else {
		if outputURL != "" {
			req, err := http.NewRequest("POST", outputURL, bytes.NewBuffer(outputBytes))

			if err != nil {
				logrus.Errorf("Error building request for output: %v", err)
				os.Exit(1)
			}

			if outputFormat == "json" {
				req.Header.Set("Content-Type", "application/json")
			} else if outputFormat == "yaml" {
				req.Header.Set("Content-Type", "application/x-yaml")
			} else {
				req.Header.Set("Content-Type", "text/plain")
			}

			client := &http.Client{}
			if skipSslValidation {
				transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
				client = &http.Client{Transport: transport}
			}
			resp, err := client.Do(req)
			if err != nil {
				logrus.Errorf("Error making request for output: %v", err)
				os.Exit(1)
			}

			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)

			if err != nil {
				logrus.Errorf("Error reading response: %v", err)
				os.Exit(1)
			}

			logrus.Infof("Received response: %v", body)
		}

		if outputFile != "" {
			err := os.WriteFile(outputFile, outputBytes, 0644)
			if err != nil {
				logrus.Errorf("Error writing output to file: %v", err)
				os.Exit(1)
			}
		}
	}
}
