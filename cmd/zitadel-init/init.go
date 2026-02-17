package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/zitadel/zitadel-go/v3/pkg/client"
	"github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/admin"
	app "github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/application/v2"
	"github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/idp"
	"github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/org/v2"
	"github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/project/v2"
	zitadeluser "github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/user/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type (
	initRunner struct {
		log           *slog.Logger
		cfg           *config
		zitadelConfig *zitadelConfig
		zitadelClient *client.Client
		kclient       ctrlclient.Client
	}

	config struct {
		pat        string
		namespace  string
		secretName string
		endpoint   string
	}

	zitadelConfig struct {
		StaticUsers []user `json:"static_users"`
		Project     struct {
			Id   string `json:"id"`
			Name string `json:"name"`
		} `json:"project"`
		Application struct {
			Id          string `json:"id"`
			Name        string `json:"name"`
			RedirectUri string `json:"redirect_uri"`
		} `json:"application"`
		GenericOIDCProviders []genericOIDCProviders `json:"generic_oidc_providers"`
	}

	user struct {
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
		Email     string `json:"email"`
		Password  string `json:"password"`
	}

	genericOIDCProviders struct {
		Name         string `json:"name"`
		Issuer       string `json:"issuer"`
		ClientId     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
)

func New(log *slog.Logger, configPath string) (*zitadelConfig, error) {
	log.Info("parsing config")

	configFile, err := os.Open(configPath)
	if err != nil {
		return nil, fmt.Errorf("unable to open config file at %s: %w", configPath, err)
	}
	defer configFile.Close()

	configData, err := io.ReadAll(configFile)
	if err != nil {
		return nil, fmt.Errorf("unable to read config file: %w", err)
	}

	var config zitadelConfig
	err = yaml.Unmarshal(configData, &config)
	if err != nil {
		return nil, fmt.Errorf("unable to parse config: %w", err)
	}

	return &config, nil
}

func NewInitRunner(log *slog.Logger, cfg *config, zitadelCfg *zitadelConfig, zitadelClient *client.Client, kclient ctrlclient.Client) *initRunner {
	return &initRunner{
		log:           log,
		cfg:           cfg,
		zitadelConfig: zitadelCfg,
		zitadelClient: zitadelClient,
		kclient:       kclient,
	}
}

func (i *initRunner) Run(ctx context.Context) error {
	i.log.Info("getting default organization")

	defaultOrgId, err := i.getDefaultOrg(ctx)
	if err != nil {
		return fmt.Errorf("unable to get default organization: %w", err)
	}

	err = i.ensureProject(ctx, defaultOrgId)
	if err != nil {
		return fmt.Errorf("unable to ensure project: %w", err)
	}

	clientId, clientSecret, err := i.ensureApp(ctx)
	if err != nil {
		return fmt.Errorf("unable to ensure application: %w", err)
	}

	err = i.createInitUsers(ctx, defaultOrgId)
	if err != nil {
		return fmt.Errorf("unable to create init users: %w", err)
	}

	err = i.createGenericOIDCProviders(ctx)
	if err != nil {
		return fmt.Errorf("unable to create generic oidc providers: %w", err)
	}

	err = i.ensureSecret(ctx, clientId, clientSecret)
	if err != nil {
		return fmt.Errorf("unable to ensure secret: %w", err)
	}

	i.log.Info("successfully initialized zitadel")

	return nil
}

func (i *initRunner) createInitUsers(ctx context.Context, orgId string) error {
	i.log.Info("creating init users")

	for _, u := range i.zitadelConfig.StaticUsers {
		i.log.Info("creating user", "user-id", u.Email)

		_, err := i.zitadelClient.UserServiceV2().CreateUser(ctx, &zitadeluser.CreateUserRequest{
			OrganizationId: orgId,
			UserId:         new(u.Email),
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
			// weird error code from zitadel api on already existing user
			if status.Code(err) != codes.FailedPrecondition {
				return fmt.Errorf("unable to create user %s: %w", u.Email, err)
			}

			_, err := i.zitadelClient.UserServiceV2().UpdateUser(ctx, &zitadeluser.UpdateUserRequest{
				Username: &u.Email,
				UserId:   u.Email,
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

	i.log.Info("successfully created init users")

	return nil
}

func (i *initRunner) createGenericOIDCProviders(ctx context.Context) error {
	i.log.Info("creating generic oidc providers")

	for _, g := range i.zitadelConfig.GenericOIDCProviders {
		ps, err := i.zitadelClient.AdminService().ListProviders(ctx, &admin.ListProvidersRequest{
			Queries: []*admin.ProviderQuery{
				{
					Query: &admin.ProviderQuery_IdpNameQuery{
						IdpNameQuery: &idp.IDPNameQuery{
							Name: g.Name,
						},
					},
				},
			},
		})
		if err != nil {
			return fmt.Errorf("unable to query generic oidc providers: %w", err)
		}

		var (
			idpID string
		)

		switch len(ps.GetResult()) {
		case 0:
			i.log.Info("creating generic oidc provider", "name", g.Name)

			idp, err := i.zitadelClient.AdminService().AddGenericOIDCProvider(ctx, &admin.AddGenericOIDCProviderRequest{
				Name:         g.Name,
				Issuer:       g.Issuer,
				ClientId:     g.ClientId,
				ClientSecret: g.ClientSecret,
				// Scopes:           []string{},
				ProviderOptions: &idp.Options{},
				// IsIdTokenMapping: false,
				// UsePkce:          false,
			})
			if err != nil {
				return fmt.Errorf("unable to add generic oidc provider %s: %w", g.Name, err)
			}

			idpID = idp.GetId()
		case 1:
			i.log.Info("updating generic oidc provider", "name", g.Name)

			idpID = ps.GetResult()[0].Id

			_, err := i.zitadelClient.AdminService().UpdateGenericOIDCProvider(ctx, &admin.UpdateGenericOIDCProviderRequest{
				Id:           idpID,
				Name:         g.Name,
				Issuer:       g.Issuer,
				ClientId:     g.ClientId,
				ClientSecret: g.ClientSecret,
				// Scopes:           []string{},
				ProviderOptions: &idp.Options{},
				// IsIdTokenMapping: false,
				// UsePkce:          false,
			})
			if err != nil {
				return fmt.Errorf("unable to update generic oidc provider %s: %w", g.Name, err)
			}
		default:
			return fmt.Errorf("multiple providers already exist for name %s", g.Name)
		}

		_, err = i.zitadelClient.AdminService().AddIDPToLoginPolicy(ctx, &admin.AddIDPToLoginPolicyRequest{
			IdpId: idpID,
		})
		if err != nil {
			if status.Code(err) != codes.AlreadyExists {
				return fmt.Errorf("unable to activate generic oidc project: %w", err)
			}

			i.log.Info("skipping activation of generic oidc provider, because already active")
		}
	}

	i.log.Info("successfully created generic oidc providers")

	return nil
}

func (i *initRunner) getDefaultOrg(ctx context.Context) (string, error) {
	orgResp, err := i.zitadelClient.OrganizationServiceV2().ListOrganizations(ctx, &org.ListOrganizationsRequest{
		Queries: []*org.SearchQuery{
			{
				Query: &org.SearchQuery_DefaultQuery{
					DefaultQuery: &org.DefaultOrganizationQuery{},
				},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("unable to get default organization: %w", err)
	}
	if len(orgResp.Result) != 1 {
		return "", fmt.Errorf("no default organization found")
	}

	return orgResp.Result[0].Id, nil
}

func (i *initRunner) ensureProject(ctx context.Context, orgId string) error {
	i.log.Info("creating project", "name", i.zitadelConfig.Project.Name)

	_, err := i.zitadelClient.ProjectServiceV2().CreateProject(ctx, &project.CreateProjectRequest{
		OrganizationId: orgId,
		ProjectId:      &i.zitadelConfig.Project.Id,
		Name:           i.zitadelConfig.Project.Name,
	})
	if err != nil {
		if status.Code(err) != codes.AlreadyExists {
			return fmt.Errorf("unable to create project: %w", err)
		}
		i.log.Info("skipping creation of project, because already existing")
		return nil
	}

	i.log.Info("successfully ensured project")

	return nil
}

func (i *initRunner) ensureApp(ctx context.Context) (clientId string, clientSecret string, err error) {
	resp, err := i.zitadelClient.ApplicationServiceV2().CreateApplication(ctx, &app.CreateApplicationRequest{
		ProjectId:     i.zitadelConfig.Project.Id,
		Name:          i.zitadelConfig.Application.Name,
		ApplicationId: i.zitadelConfig.Application.Id,
		ApplicationType: &app.CreateApplicationRequest_OidcConfiguration{
			OidcConfiguration: &app.CreateOIDCApplicationRequest{
				RedirectUris: []string{
					i.zitadelConfig.Application.RedirectUri,
				},
				ResponseTypes: []app.OIDCResponseType{
					app.OIDCResponseType_OIDC_RESPONSE_TYPE_CODE,
				},
				GrantTypes: []app.OIDCGrantType{
					app.OIDCGrantType_OIDC_GRANT_TYPE_AUTHORIZATION_CODE,
				},
				ApplicationType: app.OIDCApplicationType_OIDC_APP_TYPE_WEB,
				AuthMethodType:  app.OIDCAuthMethodType_OIDC_AUTH_METHOD_TYPE_POST,
				AccessTokenType: app.OIDCTokenType_OIDC_TOKEN_TYPE_BEARER,
				Version:         app.OIDCVersion_OIDC_VERSION_1_0,
			},
		},
	})
	if err != nil {
		if status.Code(err) != codes.AlreadyExists {
			return "", "", fmt.Errorf("unable to create application: %w", err)
		}

		resp, err := i.zitadelClient.ApplicationServiceV2().ListApplications(ctx, &app.ListApplicationsRequest{
			Filters: []*app.ApplicationSearchFilter{{
				Filter: &app.ApplicationSearchFilter_NameFilter{
					NameFilter: &app.ApplicationNameFilter{
						Name: i.zitadelConfig.Application.Name,
					},
				},
			}},
		})
		if err != nil {
			return "", "", fmt.Errorf("unable to get applications: %w", err)
		}

		if len(resp.Applications) != 1 {
			return "", "", fmt.Errorf("unable to find application %s", i.zitadelConfig.Application.Name)
		}

		// needs to be fixed to static id, after zitadel api bug is  -> remove this line then
		i.zitadelConfig.Application.Id = resp.Applications[0].ApplicationId

		// USE GET INSTEAD OF LIST+FILTER WHEN ZITADEL API FIXED
		// getResp, err := zitadelClient.ApplicationServiceV2().GetApplication(ctx, &app.GetApplicationRequest{
		// 	ApplicationId: "metal-stack",
		// })
		// if err != nil {
		// 	return fmt.Errorf("unable to get application %s: %w", "metal-stack", err)
		// }

		_, err = i.zitadelClient.ApplicationServiceV2().UpdateApplication(ctx, &app.UpdateApplicationRequest{
			ProjectId:     i.zitadelConfig.Project.Id,
			ApplicationId: i.zitadelConfig.Application.Id,
			ApplicationType: &app.UpdateApplicationRequest_OidcConfiguration{
				OidcConfiguration: &app.UpdateOIDCApplicationConfigurationRequest{
					RedirectUris: []string{
						i.zitadelConfig.Application.RedirectUri,
					},
				},
			},
		})
		if err != nil {
			if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "No changes") {
				return "", "", fmt.Errorf("unable to update application: %w", err)
			}
		}

		clientId = resp.Applications[0].GetOidcConfiguration().ClientId
	} else {
		// needs to be fixed to static id, after zitadel api bug is  -> remove this line then
		i.zitadelConfig.Application.Id = resp.ApplicationId

		i.log.Info("successfully created application", "app-id", resp.ApplicationId)

		oidc := resp.GetOidcConfiguration()
		if oidc == nil {
			return "", "", fmt.Errorf("no oidc response found in app creation response")
		}

		clientId = oidc.GetClientId()
		clientSecret = oidc.GetClientSecret()
	}

	return clientId, clientSecret, nil
}

func (i *initRunner) ensureSecret(ctx context.Context, clientId, clientSecret string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      i.cfg.secretName,
			Namespace: i.cfg.namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, i.kclient, secret, func() error {
		secret.Type = corev1.SecretTypeOpaque
		if clientSecret != "" {
			secret.StringData = map[string]string{
				"client_id":     clientId,
				"client_secret": clientSecret,
			}
			return nil
		}

		if secret.Data == nil || len(secret.Data["client_secret"]) == 0 {
			i.log.Info("regenerating client secret")
			resp, err := i.zitadelClient.ApplicationServiceV2().GenerateClientSecret(ctx, &app.GenerateClientSecretRequest{
				ProjectId:     i.zitadelConfig.Project.Id,
				ApplicationId: i.zitadelConfig.Application.Id,
			})
			if err != nil {
				return fmt.Errorf("unable to regenerate client secret: %w", err)
			}

			clientSecret = resp.ClientSecret
		}

		secret.StringData = map[string]string{
			"client_id":     clientId,
			"client_secret": clientSecret,
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("unable to save credentials in secret: %w", err)
	}

	return nil
}
