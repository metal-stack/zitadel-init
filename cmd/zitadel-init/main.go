package main

import (
	"context"
	"log"
	"os"

	"github.com/urfave/cli/v3"
)

var (
	zitadelEndpoint = &cli.StringFlag{
		Name:  "zitadel-endpoint",
		Value: "zitadel.172.17.0.1.nip.io",
		Usage: "Zitadel server address",
	}
	zitadelPAT = &cli.StringFlag{
		Name:  "zitadel-pat",
		Value: "your-personal-access-token",
		Usage: "Zitadel personal access token",
	}
	zitadelPort = &cli.Uint16Flag{
		Name:  "zitadel-port",
		Value: 4443,
		Usage: "Zitadel server port",
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
)

func main() {
	init := &cli.Command{
		Name:  "zitadel-init",
		Usage: "Initialize Zitadel with required applications",
		Flags: []cli.Flag{
			zitadelEndpoint,
			zitadelPAT,
			zitadelPort,
			secretNamespace,
			secretName,
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			return runInit(ctx, c)
		},
	}

	if err := init.Run(context.Background(), os.Args); err != nil {
		log.Fatal(err)
	}
}
