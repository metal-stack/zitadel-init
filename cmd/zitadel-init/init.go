package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	"github.com/zitadel/zitadel-go/v3/pkg/client"
	app "github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/app/v2beta"
	project "github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/project/v2beta"
	zitadeluser "github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/user/v2"
	"github.com/zitadel/zitadel-go/v3/pkg/zitadel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

type user struct {
	OrgID     string `json:"org_id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Email     string `json:"email"`
	Password  string `json:"password"`
}

func runInit(ctx context.Context, cmd *cli.Command, log *slog.Logger) error {
	var (
		domain          = cmd.String(zitadelEndpoint.Name)
		patSecretName   = cmd.String(zitadelCredentialsSecretName.Name)
		usersSecretName = cmd.String(zitadelCredentialsSecretName.Name)
		port            = cmd.Uint16(zitadelPort.Name)
		skipVerifyTLS   = cmd.Bool(zitadelSkipVerifyTLS.Name)
		insecure        = cmd.Bool(zitadelInsecure.Name)
		namespace       = cmd.String(secretNamespace.Name)
		secretName      = cmd.String(secretName.Name)

		opts = []zitadel.Option{zitadel.WithPort(port)}
	)

	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("unable to get in-cluster config: %w", err)
	}

	c, err := ctrlclient.New(config, ctrlclient.Options{})
	if err != nil {
		return fmt.Errorf("unable to create kubernetes client: %w", err)
	}

	log.Info("waiting for pat secret to come into existence...")

	var pat string

	for {
		patSecret := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      patSecretName,
				Namespace: namespace,
			},
		}

		err = c.Get(ctx, ctrlclient.ObjectKeyFromObject(&patSecret), &patSecret)
		if err == nil {
			pat = strings.TrimSpace(string(patSecret.Data["pat"]))
			break
		}

		if apierrors.IsNotFound(err) {
			time.Sleep(2 * time.Second)
			continue
		}

		return fmt.Errorf("error retrieving pat secret: %w", err)
	}

	if pat == "" {
		return fmt.Errorf("pat secret is empty")
	}

	log.Info("initializing zitadel application...")

	if skipVerifyTLS {
		opts = append(opts, zitadel.WithInsecureSkipVerifyTLS())
	}
	if insecure {
		opts = append(opts, zitadel.WithInsecure(strconv.Itoa(int(port))))
	}

	api, err := client.New(ctx, zitadel.New(domain, opts...), client.WithAuth(client.PAT(pat)))
	if err != nil {
		return fmt.Errorf("unable to create API client: %w", err)
	}

	projectResp, err := api.ProjectServiceV2Beta().ListProjects(ctx, &project.ListProjectsRequest{})
	if err != nil {
		return fmt.Errorf("unable to list projects: %w", err)
	}

	// Use the first project found
	if len(projectResp.Projects) == 0 {
		return fmt.Errorf("no projects found")
	}
	projectId := projectResp.Projects[0].Id

	resp, err := api.AppServiceV2Beta().CreateApplication(ctx, &app.CreateApplicationRequest{
		ProjectId: projectId,
		Name:      "metal-stack",
		Id:        "metal-stack",
		CreationRequestType: &app.CreateApplicationRequest_OidcRequest{
			OidcRequest: &app.CreateOIDCApplicationRequest{
				RedirectUris: []string{
					"http://v2.api.172.17.0.1.nip.io:8080/auth/openid-connect/callback",
				},
				ResponseTypes: []app.OIDCResponseType{
					app.OIDCResponseType_OIDC_RESPONSE_TYPE_CODE,
				},
				GrantTypes: []app.OIDCGrantType{
					app.OIDCGrantType_OIDC_GRANT_TYPE_AUTHORIZATION_CODE,
				},
				AppType:                app.OIDCAppType_OIDC_APP_TYPE_WEB,
				AuthMethodType:         app.OIDCAuthMethodType_OIDC_AUTH_METHOD_TYPE_POST,
				AccessTokenType:        app.OIDCTokenType_OIDC_TOKEN_TYPE_BEARER,
				Version:                app.OIDCVersion_OIDC_VERSION_1_0,
				PostLogoutRedirectUris: []string{},
				DevMode:                true,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("unable to create application: %w", err)
	}

	log.Info("successfully created application", "app-id", resp.AppId)

	oidc := resp.GetOidcResponse()
	if oidc == nil {
		return fmt.Errorf("no oidc response found in app creation response")
	}

	usersSecret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      usersSecretName,
			Namespace: namespace,
		},
	}

	err = c.Get(ctx, ctrlclient.ObjectKeyFromObject(&usersSecret), &usersSecret)
	if err == nil {
		var users []user
		err = json.Unmarshal(usersSecret.Data["users"], &users)
		if err != nil {
			return fmt.Errorf("unable to parse users: %w", err)
		}

		for _, u := range users {
			api.UserServiceV2().CreateUser(ctx, &zitadeluser.CreateUserRequest{
				OrganizationId: u.OrgID,
				UserType: &zitadeluser.CreateUserRequest_Human_{
					Human: &zitadeluser.CreateUserRequest_Human{
						Profile: &zitadeluser.SetHumanProfile{
							GivenName:  u.FirstName,
							FamilyName: u.LastName,
						},
						Email: &zitadeluser.SetHumanEmail{
							Email: u.Email,
						},
						PasswordType: &zitadeluser.CreateUserRequest_Human_Password{
							Password: &zitadeluser.Password{
								Password:       u.Password,
								ChangeRequired: false,
							},
						},
					},
				},
			})
		}
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
		StringData: map[string]string{
			"client_id":     resp.GetOidcResponse().GetClientId(),
			"client_secret": resp.GetOidcResponse().GetClientSecret(),
		},
		Type: corev1.SecretTypeOpaque,
	}

	err = c.Create(ctx, secret)
	if err != nil {
		return fmt.Errorf("unable to save credentials in secret: %w", err)
	}

	log.Info("successfully created zitadel-client-credentials")

	return nil
}
