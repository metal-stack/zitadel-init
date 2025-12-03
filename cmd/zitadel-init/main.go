package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/urfave/cli/v3"
)

var (
	zitadelEndpoint = &cli.StringFlag{
		Name:  "zitadel-endpoint",
		Value: "zitadel.172.17.0.1.nip.io",
		Usage: "Zitadel server address",
	}
	zitadelCredentialsSecretName = &cli.StringFlag{
		Name:  "zitadel-credentials-secret-name",
		Value: "iam-admin-pat",
		Usage: "the secret name containing the pat credentials",
	}
	zitadelPort = &cli.Uint16Flag{
		Name:  "zitadel-port",
		Value: 4443,
		Usage: "Zitadel server port",
	}
	zitadelSkipVerifyTLS = &cli.BoolFlag{
		Name:  "zitadel-skip-verify-tls",
		Value: false,
		Usage: "Allows to connect to an instance running with TLS but has an untrusted certificate",
	}
	zitadelInsecure = &cli.BoolFlag{
		Name:  "zitadel-insecure",
		Value: false,
		Usage: "allows to connect to an instance running without TLS, do not use in production",
	}
	secretNamespace = &cli.StringFlag{
		Name:  "namespace",
		Value: "metal-control-plane",
		Usage: "Namespace for the client secret",
	}
	secretName = &cli.StringFlag{
		Name:  "secret",
		Value: "zitadel-client-credentials",
		Usage: "Namespace for the client secret",
	}
	initialUsersSecretName = &cli.StringFlag{
		Name:  "initial-users-secret-name",
		Value: "zitadel-static-users",
		Usage: "the secret name containing the static users to create",
	}
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	init := &cli.Command{
		Name:  "zitadel-init",
		Usage: "Initialize Zitadel with required applications",
		Flags: []cli.Flag{
			zitadelEndpoint,
			zitadelCredentialsSecretName,
			zitadelPort,
			zitadelSkipVerifyTLS,
			zitadelInsecure,
			secretNamespace,
			secretName,
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			return runInit(ctx, c, log)
		},
	}

	if err := init.Run(context.Background(), os.Args); err != nil {
		log.Error("error running init, shutting down", "error", err)
		os.Exit(1)
	}
}
