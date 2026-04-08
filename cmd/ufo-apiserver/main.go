package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/options"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/component-base/cli"
	basecompatibility "k8s.io/component-base/compatibility"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/klog/v2"

	ufoapiserver "go.datum.net/ufo/internal/apiserver"

	// Register JSON logging format.
	_ "k8s.io/component-base/logs/json/register"
)

func init() {
	utilruntime.Must(logsapi.AddFeatureGates(utilfeature.DefaultMutableFeatureGate))
	utilfeature.DefaultMutableFeatureGate.Set("LoggingBetaOptions=true")
	utilfeature.DefaultMutableFeatureGate.Set("RemoteRequestHeaderUID=true")
}

func main() {
	cmd := newRootCommand()
	code := cli.Run(cmd)
	os.Exit(code)
}

func newRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ufo-apiserver",
		Short: "ufo-apiserver - Kubernetes aggregated API server for Unikraft Functions",
	}
	cmd.AddCommand(newServeCommand())
	return cmd
}

func newServeCommand() *cobra.Command {
	opts := newServerOptions()

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the ufo API server",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := opts.Complete(); err != nil {
				return err
			}
			if err := opts.Validate(); err != nil {
				return err
			}
			return run(opts, cmd.Context())
		},
	}

	flags := cmd.Flags()
	opts.AddFlags(flags)
	logsapi.AddFlags(opts.Logs, flags)

	return cmd
}

// serverOptions holds configuration for the ufo API server.
type serverOptions struct {
	RecommendedOptions *options.RecommendedOptions
	Logs               *logsapi.LoggingConfiguration
}

func newServerOptions() *serverOptions {
	return &serverOptions{
		RecommendedOptions: options.NewRecommendedOptions(
			"/registry/functions.datumapis.com",
			ufoapiserver.Codecs.LegacyCodec(ufoapiserver.Scheme.PrioritizedVersionsAllGroups()...),
		),
		Logs: logsapi.NewLoggingConfiguration(),
	}
}

func (o *serverOptions) AddFlags(fs *pflag.FlagSet) {
	o.RecommendedOptions.AddFlags(fs)
}

func (o *serverOptions) Complete() error {
	return nil
}

func (o *serverOptions) Validate() error {
	return nil
}

func (o *serverOptions) Config() (*ufoapiserver.Config, error) {
	if err := o.RecommendedOptions.SecureServing.MaybeDefaultWithSelfSignedCerts(
		"localhost", nil, nil); err != nil {
		return nil, fmt.Errorf("error creating self-signed certificates: %v", err)
	}

	genericConfig := genericapiserver.NewRecommendedConfig(ufoapiserver.Codecs)
	genericConfig.EffectiveVersion = basecompatibility.NewEffectiveVersionFromString("1.34", "", "")

	if err := o.RecommendedOptions.ApplyTo(genericConfig); err != nil {
		return nil, fmt.Errorf("failed to apply recommended options: %w", err)
	}

	return &ufoapiserver.Config{
		GenericConfig: genericConfig,
	}, nil
}

func run(opts *serverOptions, ctx context.Context) error {
	if err := logsapi.ValidateAndApply(opts.Logs, utilfeature.DefaultMutableFeatureGate); err != nil {
		return fmt.Errorf("failed to apply logging configuration: %w", err)
	}

	config, err := opts.Config()
	if err != nil {
		return err
	}

	server, err := config.Complete().New()
	if err != nil {
		return err
	}

	defer logs.FlushLogs()

	klog.Info("Starting ufo-apiserver...")
	return server.GenericAPIServer.PrepareRun().RunWithContext(ctx)
}
