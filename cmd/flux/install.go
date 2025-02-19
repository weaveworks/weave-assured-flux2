/*
Copyright 2020 The Flux authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/fluxcd/flux2/v2/internal/flags"
	"github.com/fluxcd/flux2/v2/internal/utils"
	"github.com/fluxcd/flux2/v2/pkg/manifestgen"
	"github.com/fluxcd/flux2/v2/pkg/manifestgen/install"
	"github.com/fluxcd/flux2/v2/pkg/status"
)

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install or upgrade Flux",
	Long: `The install command deploys Flux in the specified namespace.
If a previous version is installed, then an in-place upgrade will be performed.`,
	Example: `  # Install the latest version in the flux-system namespace
  flux install --namespace=flux-system

  # Install a specific series of components
  flux install --components="source-controller,kustomize-controller"

  # Install all components including the image automation ones
  flux install --components-extra="image-reflector-controller,image-automation-controller"

  # Install Flux onto tainted Kubernetes nodes
  flux install --toleration-keys=node.kubernetes.io/dedicated-to-flux

  # Dry-run install
  flux install --export | kubectl apply --dry-run=client -f- 

  # Write install manifests to file
  flux install --export > flux-system.yaml`,
	RunE: installCmdRun,
}

type installFlags struct {
	export             bool
	version            string
	defaultComponents  []string
	extraComponents    []string
	registry           string
	assured            bool
	imagePullSecret    string
	branch             string
	watchAllNamespaces bool
	networkPolicy      bool
	manifestsPath      string
	logLevel           flags.LogLevel
	tokenAuth          bool
	clusterDomain      string
	tolerationKeys     []string
}

var installArgs = NewInstallFlags()

func init() {
	installCmd.Flags().BoolVar(&installArgs.export, "export", false,
		"write the install manifests to stdout and exit")
	installCmd.Flags().StringVarP(&installArgs.version, "version", "v", "",
		"toolkit version, when specified the manifests are downloaded from https://github.com/fluxcd/flux2/releases")
	installCmd.Flags().StringSliceVar(&installArgs.defaultComponents, "components", rootArgs.defaults.Components,
		"list of components, accepts comma-separated values")
	installCmd.Flags().StringSliceVar(&installArgs.extraComponents, "components-extra", nil,
		"list of components in addition to those supplied or defaulted, accepts values such as 'image-reflector-controller,image-automation-controller'")
	installCmd.Flags().StringVar(&installArgs.manifestsPath, "manifests", "", "path to the manifest directory")
	installCmd.Flags().StringVar(&installArgs.registry, "registry", "", "container registry where the toolkit images are published")
	installCmd.Flags().BoolVar(&installArgs.assured, "assured", false, "use weave-assured container images from the registry")
	installCmd.Flags().StringVar(&installArgs.imagePullSecret, "image-pull-secret", "",
		"Kubernetes secret name used for pulling the toolkit images from a private registry")
	installCmd.Flags().BoolVar(&installArgs.watchAllNamespaces, "watch-all-namespaces", rootArgs.defaults.WatchAllNamespaces,
		"watch for custom resources in all namespaces, if set to false it will only watch the namespace where the toolkit is installed")
	installCmd.Flags().Var(&installArgs.logLevel, "log-level", installArgs.logLevel.Description())
	installCmd.Flags().BoolVar(&installArgs.networkPolicy, "network-policy", rootArgs.defaults.NetworkPolicy,
		"deny ingress access to the toolkit controllers from other namespaces using network policies")
	installCmd.Flags().StringVar(&installArgs.clusterDomain, "cluster-domain", rootArgs.defaults.ClusterDomain, "internal cluster domain")
	installCmd.Flags().StringSliceVar(&installArgs.tolerationKeys, "toleration-keys", nil,
		"list of toleration keys used to schedule the components pods onto nodes with matching taints")
	installCmd.Flags().MarkHidden("manifests")

	rootCmd.AddCommand(installCmd)
}

func NewInstallFlags() installFlags {
	return installFlags{
		logLevel: flags.LogLevel(rootArgs.defaults.LogLevel),
	}
}

func installCmdRun(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), rootArgs.timeout)
	defer cancel()

	components := append(installArgs.defaultComponents, installArgs.extraComponents...)
	err := utils.ValidateComponents(components)
	if err != nil {
		return err
	}

	if ver, err := getVersion(installArgs.version); err != nil {
		return err
	} else {
		installArgs.version = ver
	}

	if !installArgs.export {
		logger.Generatef("generating manifests")
	}

	tmpDir, err := manifestgen.MkdirTempAbs("", *kubeconfigArgs.Namespace)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	manifestsBase := ""
	if isEmbeddedVersion(installArgs.version) {
		if err := writeEmbeddedManifests(tmpDir); err != nil {
			return err
		}
		manifestsBase = tmpDir
	}

	opts := install.Options{
		BaseURL:                installArgs.manifestsPath,
		Version:                installArgs.version,
		Namespace:              *kubeconfigArgs.Namespace,
		Components:             components,
		Registry:               installRegistry(),
		ImagePullSecret:        installArgs.imagePullSecret,
		WatchAllNamespaces:     installArgs.watchAllNamespaces,
		NetworkPolicy:          installArgs.networkPolicy,
		LogLevel:               installArgs.logLevel.String(),
		NotificationController: rootArgs.defaults.NotificationController,
		ManifestFile:           fmt.Sprintf("%s.yaml", *kubeconfigArgs.Namespace),
		Timeout:                rootArgs.timeout,
		ClusterDomain:          installArgs.clusterDomain,
		TolerationKeys:         installArgs.tolerationKeys,
	}

	if installArgs.manifestsPath == "" {
		opts.BaseURL = install.MakeDefaultOptions().BaseURL
		if installArgs.assured {
			opts.BaseURL = "https://github.com/weaveworks/weave-assured-flux2/releases"
		}
	}

	manifest, err := install.Generate(opts, manifestsBase)
	if err != nil {
		return fmt.Errorf("install failed: %w", err)
	}

	if _, err := manifest.WriteFile(tmpDir); err != nil {
		return fmt.Errorf("install failed: %w", err)
	}

	if installArgs.export {
		fmt.Print(manifest.Content)
		return nil
	} else if rootArgs.verbose {
		fmt.Print(manifest.Content)
	}

	logger.Successf("manifests build completed")
	logger.Actionf("installing components in %s namespace", *kubeconfigArgs.Namespace)

	applyOutput, err := utils.Apply(ctx, kubeconfigArgs, kubeclientOptions, tmpDir, filepath.Join(tmpDir, manifest.Path))
	if err != nil {
		return fmt.Errorf("install failed: %w", err)
	}

	fmt.Fprintln(os.Stderr, applyOutput)

	kubeConfig, err := utils.KubeConfig(kubeconfigArgs, kubeclientOptions)
	if err != nil {
		return fmt.Errorf("install failed: %w", err)
	}
	statusChecker, err := status.NewStatusChecker(kubeConfig, 5*time.Second, rootArgs.timeout, logger)
	if err != nil {
		return fmt.Errorf("install failed: %w", err)
	}
	componentRefs, err := buildComponentObjectRefs(components...)
	if err != nil {
		return fmt.Errorf("install failed: %w", err)
	}
	logger.Waitingf("verifying installation")
	if err := statusChecker.Assess(componentRefs...); err != nil {
		return fmt.Errorf("install failed")
	}

	logger.Successf("install finished")
	return nil
}

func installRegistry() string {
	if installArgs.registry == "" {
		installArgs.registry = rootArgs.defaults.Registry
		if installArgs.assured {
			installArgs.registry = "ghcr.io/weaveworks"
		}
	}
	return installArgs.registry
}
