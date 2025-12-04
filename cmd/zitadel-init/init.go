package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"

	"github.com/urfave/cli/v3"
	"github.com/zitadel/zitadel-go/v3/pkg/client"
	app "github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/app/v2beta"
	project "github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/project/v2beta"
	zitadeluser "github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/user/v2"
	"github.com/zitadel/zitadel-go/v3/pkg/zitadel"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
		domain        = cmd.String(zitadelEndpoint.Name)
		port          = cmd.Uint16(zitadelPort.Name)
		skipVerifyTLS = cmd.Bool(zitadelSkipVerifyTLS.Name)
		insecure      = cmd.Bool(zitadelInsecure.Name)
		namespace     = cmd.String(secretNamespace.Name)
		secretName    = cmd.String(secretName.Name)
		pat           = cmd.String(zitadelPAT.Name)
		usersPath     = cmd.String(initialUsersPath.Name)

		opts = []zitadel.Option{zitadel.WithPort(port)}

		clientId     string
		clientSecret string
	)
	log.Info("initializing zitadel application...")

	if skipVerifyTLS {
		opts = append(opts, zitadel.WithInsecureSkipVerifyTLS())
	}
	if insecure {
		opts = append(opts, zitadel.WithInsecure(strconv.Itoa(int(port))))
	}

	zitadelClient, err := client.New(ctx, zitadel.New(domain, opts...), client.WithAuth(client.PAT(pat)))
	if err != nil {
		return fmt.Errorf("unable to create API client: %w", err)
	}

	projectResp, err := zitadelClient.ProjectServiceV2Beta().ListProjects(ctx, &project.ListProjectsRequest{})
	if err != nil {
		return fmt.Errorf("unable to list projects: %w", err)
	}

	// Use the first project found
	if len(projectResp.Projects) == 0 {
		return fmt.Errorf("no projects found")
	}
	projectId := projectResp.Projects[0].Id

	resp, err := zitadelClient.AppServiceV2Beta().CreateApplication(ctx, &app.CreateApplicationRequest{
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
			},
		},
	})
	if err != nil {
		if status.Code(err) != codes.AlreadyExists {
			return fmt.Errorf("unable to create application: %w", err)
		}

		_, err := zitadelClient.AppServiceV2Beta().UpdateApplication(ctx, &app.UpdateApplicationRequest{
			ProjectId: projectId,
			Name:      "metal-stack",
			Id:        "metal-stack",
			UpdateRequestType: &app.UpdateApplicationRequest_OidcConfigurationRequest{
				OidcConfigurationRequest: &app.UpdateOIDCApplicationConfigurationRequest{
					RedirectUris: []string{
						"http://v2.api.172.17.0.1.nip.io:8080/auth/openid-connect/callback",
					},
				},
			},
		})
		if err != nil {
			return fmt.Errorf("unable to update application: %w", err)
		}

		resp, err := zitadelClient.AppServiceV2Beta().GetApplication(ctx, &app.GetApplicationRequest{
			Id: "metal-stack",
		})
		if err != nil {
			return fmt.Errorf("unable to get updated application: %w", err)
		}

		clientId = resp.GetApp().GetApiConfig().ClientId
	} else {
		log.Info("successfully created application", "app-id", resp.AppId)

		oidc := resp.GetOidcResponse()
		if oidc == nil {
			return fmt.Errorf("no oidc response found in app creation response")
		}

		clientId = oidc.GetClientId()
		clientSecret = oidc.GetClientSecret()
	}

	if usersPath != "" {
		err = createInitUsers(ctx, log, usersPath, zitadelClient)
		if err != nil {
			return fmt.Errorf("unable to create inti users: %w", err)
		}
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("unable to get in-cluster config: %w", err)
	}

	c, err := ctrlclient.New(config, ctrlclient.Options{})
	if err != nil {
		return fmt.Errorf("unable to create kubernetes client: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
	}

	controllerutil.CreateOrUpdate(ctx, c, secret, func() error {
		secret.Type = corev1.SecretTypeOpaque
		if clientSecret != "" {
			secret.StringData = map[string]string{
				"client_id":     clientId,
				"client_secret": clientSecret,
			}
			return nil
		}

		resp, err := zitadelClient.AppServiceV2Beta().RegenerateClientSecret(ctx, &app.RegenerateClientSecretRequest{
			ProjectId:     "metal-stack",
			ApplicationId: "metal-stack",
			AppType: &app.RegenerateClientSecretRequest_IsOidc{
				IsOidc: true,
			},
		})
		if err != nil {
			return fmt.Errorf("unable to regenerate client secret: %w", err)
		}

		secret.StringData = map[string]string{
			"client_id":     clientId,
			"client_secret": resp.ClientSecret,
		}
		return nil
	})
	err = c.Create(ctx, secret)
	if err != nil {
		return fmt.Errorf("unable to save credentials in secret: %w", err)
	}

	log.Info("successfully created zitadel-client-credentials")

	return nil
}

func createInitUsers(ctx context.Context, log *slog.Logger, usersPath string, zitadelClient *client.Client) error {
	usersFile, err := os.Open(usersPath)
	if err != nil {
		return fmt.Errorf("unable to open users file at %s: %w", usersPath, err)
	}
	defer usersFile.Close()

	usersData, err := io.ReadAll(usersFile)
	if err != nil {
		return fmt.Errorf("unable to read users file: %w", err)
	}

	var users []user
	err = json.Unmarshal(usersData, &users)
	if err != nil {
		return fmt.Errorf("unable to parse users: %w", err)
	}

	for _, u := range users {
		_, err := zitadelClient.UserServiceV2().CreateUser(ctx, &zitadeluser.CreateUserRequest{
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
		if err != nil {
			if status.Code(err) != codes.AlreadyExists {
				return fmt.Errorf("unable to create user %s: %w", u.Email, err)
			}

			_, err := zitadelClient.UserServiceV2().UpdateUser(ctx, &zitadeluser.UpdateUserRequest{
				Username: &u.Email,
				UserType: &zitadeluser.UpdateUserRequest_Human_{
					Human: &zitadeluser.UpdateUserRequest_Human{
						Profile: &zitadeluser.UpdateUserRequest_Human_Profile{
							GivenName:  &u.FirstName,
							FamilyName: &u.LastName,
						},
					},
				},
			})
			if err != nil {
				return fmt.Errorf("unable to update user %s: %w", u.Email, err)
			}
		}
	}

	log.Info("successfully created init users")

	return nil
}
