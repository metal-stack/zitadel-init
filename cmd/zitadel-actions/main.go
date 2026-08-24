package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/urfave/cli/v3"
)

var (
	bindAddr = &cli.StringFlag{
		Name:  "bind-addr",
		Value: "0.0.0.0:8443",
		Usage: "address the action server should listen on",
	}
	signingKey = &cli.StringFlag{
		Name:     "signing-key",
		Value:    "",
		Usage:    "signing key returned by Zitadel when the target was created",
		Required: true,
	}
	allowRoles = &cli.StringSliceFlag{
		Name:  "allow-roles",
		Value: nil,
		Usage: "roles that must be contained in the idp role claims, otherwise login is prevented",
	}
	clientIDs = &cli.StringSliceFlag{
		Name:  "client-ids",
		Value: nil,
		Usage: "only enforce the role policy for these client_ids; if unset the policy applies to all applications",
	}
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	server := &cli.Command{
		Name:  "actions-server",
		Usage: "V2 actions server",
		Flags: []cli.Flag{
			bindAddr,
			signingKey,
			allowRoles,
			clientIDs,
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			h := &handler{
				log:          log,
				allowedRoles: c.StringSlice(allowRoles.Name),
				clientIDs:    c.StringSlice(clientIDs.Name),
				signingKey:   c.String(signingKey.Name),
			}

			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				h.handle(w, r)
			})

			addr := c.String(bindAddr.Name)
			log.Info("starting zitadel actions server", "listen", addr)

			httpServer := &http.Server{
				Addr:    addr,
				Handler: mux,
			}

			errCh := make(chan error, 1)
			go func() {
				errCh <- httpServer.ListenAndServe()
			}()

			select {
			case err := <-errCh:
				return fmt.Errorf("http server failed: %w", err)
			case <-ctx.Done():
				log.Info("shutting down zitadel actions server")
				return httpServer.Shutdown(context.Background())
			}
		},
	}

	if err := server.Run(context.Background(), os.Args); err != nil {
		log.Error("error running action server, shutting down", "error", err)
		os.Exit(1)
	}
}
