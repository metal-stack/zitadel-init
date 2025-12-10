package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/urfave/cli/v3"
	"github.com/zitadel/zitadel-go/v3/pkg/client"
	"github.com/zitadel/zitadel-go/v3/pkg/zitadel"
	"google.golang.org/grpc"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
)

var (
	zitadelEndpoint = &cli.StringFlag{
		Name:  "zitadel-endpoint",
		Value: "localhost",
		Usage: "zitadel server address",
	}
	zitadelExternalDomain = &cli.StringFlag{
		Name:  "zitadel-external-domain",
		Value: "",
		Usage: "if defined overwrites the authorization pseudo-header for the tls authorization handshake during grpc dial",
	}
	zitadelPAT = &cli.StringFlag{
		Name:  "zitadel-pat",
		Value: "your-personal-access-token",
		Usage: "personal access token for Zitadel",
	}
	zitadelPort = &cli.Uint16Flag{
		Name:  "zitadel-port",
		Value: 8080,
		Usage: "zitadel server port",
	}
	zitadelSkipVerifyTLS = &cli.BoolFlag{
		Name:  "zitadel-skip-verify-tls",
		Value: false,
		Usage: "allows to connect to an instance running with TLS but has an untrusted certificate",
	}
	zitadelInsecure = &cli.BoolFlag{
		Name:  "zitadel-insecure",
		Value: true,
		Usage: "allows to connect to an instance running without TLS, do not use in production",
	}
	secretNamespace = &cli.StringFlag{
		Name:  "namespace",
		Value: "metal-control-plane",
		Usage: "namespace for the client secret",
	}
	secretName = &cli.StringFlag{
		Name:  "secret",
		Value: "zitadel-client-credentials",
		Usage: "namespace for the client secret",
	}
	configPath = &cli.StringFlag{
		Name:  "config-path",
		Value: "",
		Usage: "path of the config path",
	}
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	init := &cli.Command{
		Name:  "zitadel-init",
		Usage: "Initialize Zitadel with required applications",
		Flags: []cli.Flag{
			zitadelEndpoint,
			zitadelPAT,
			zitadelPort,
			zitadelSkipVerifyTLS,
			zitadelInsecure,
			zitadelExternalDomain,
			secretNamespace,
			secretName,
			configPath,
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			var (
				endpoint       = c.String(zitadelEndpoint.Name)
				externalDomain = c.String(zitadelExternalDomain.Name)
				port           = c.Uint16(zitadelPort.Name)
				skipVerifyTLS  = c.Bool(zitadelSkipVerifyTLS.Name)
				insecure       = c.Bool(zitadelInsecure.Name)
				namespace      = c.String(secretNamespace.Name)
				secretName     = c.String(secretName.Name)
				pat            = c.String(zitadelPAT.Name)
				configPath     = c.String(configPath.Name)

				opts = []zitadel.Option{zitadel.WithPort(port)}
			)

			zitadelConfig, err := New(log, configPath)
			if err != nil {
				return fmt.Errorf("unable to create zitadel config: %w", err)
			}

			config := &config{
				pat:        pat,
				namespace:  namespace,
				secretName: secretName,
			}

			k8sConfig, err := ctrlconfig.GetConfig()
			if err != nil {
				return fmt.Errorf("unable to get kubeconfig: %w", err)
			}

			kclient, err := ctrlclient.New(k8sConfig, ctrlclient.Options{})
			if err != nil {
				return fmt.Errorf("unable to create kubernetes client: %w", err)
			}

			if skipVerifyTLS {
				opts = append(opts, zitadel.WithInsecureSkipVerifyTLS())
			}
			if insecure {
				opts = append(opts, zitadel.WithInsecure(strconv.Itoa(int(port))))
			}

			authority := endpoint
			if externalDomain != "" {
				authority = externalDomain
			}

			zitadelClient, err := client.New(ctx, zitadel.New(endpoint, opts...), client.WithAuth(client.PAT(pat)), client.WithGRPCDialOptions(grpc.WithAuthority(authority)))
			if err != nil {
				return fmt.Errorf("unable to create API client: %w", err)
			}

			initRunner := NewInitRunner(log, config, zitadelConfig, zitadelClient, kclient)
			err = initRunner.Run(ctx)
			if err != nil {
				return fmt.Errorf("unable to execute init runner: %w", err)
			}

			return nil
		},
	}

	if err := init.Run(context.Background(), os.Args); err != nil {
		log.Error("error running init, shutting down", "error", err)
		os.Exit(1)
	}
}
