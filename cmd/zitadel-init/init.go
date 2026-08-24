package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/zitadel/zitadel-go/v3/pkg/client"
	"github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/action/v2"
	"github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/admin"
	app "github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/application/v2"
	"github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/idp"
	"github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/org/v2"
	"github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/project/v2"
	zitadeluser "github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/user/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
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
		pat               string
		namespace         string
		secretName        string
		actionsSecretName string
	}

	zitadelConfig struct {
		StaticUsers []user `json:"static_users"`
		Project     struct {
			Id   string `json:"id"`
			Name string `json:"name"`
		} `json:"project"`
		Application struct {
			Id   string `json:"id"`
			Name string `json:"name"`
			// Deprecated: Use RedirectUris instead, this is for now added to the slice to ensure backward compatibility
			RedirectUri  string   `json:"redirect_uri"`
			RedirectUris []string `json:"redirect_uris"`
		} `json:"application"`
		GenericOIDCProviders []genericOIDCProviders `json:"generic_oidc_providers"`
		ActionsTarget        actionsTarget          `json:"actions_target"`
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
		IsAutoCreate bool   `json:"is_auto_create"`
		IsAutoUpdate bool   `json:"is_auto_update"`
	}

	actionsTarget struct {
		Name     string `json:"name"`
		Endpoint string `json:"endpoint"`
		Function string `json:"function"`
	}
)

func New(log *slog.Logger, configPath string) (*zitadelConfig, error) {
	log.Info("parsing config")

	configFile, err := os.Open(configPath)
	if err != nil {
		return nil, fmt.Errorf("unable to open config file at %s: %w", configPath, err)
	}
	defer func() {
		if err := configFile.Close(); err != nil {
			log.Error("unable to close config file", "error", err)
		}
	}()

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

	targetID, signingKey, err := i.ensureActionsTarget(ctx)
	if err != nil {
		return fmt.Errorf("unable to ensure actions target: %w", err)
	}

	err = i.ensureActionsExecution(ctx, targetID)
	if err != nil {
		return fmt.Errorf("unable to ensure actions execution: %w", err)
	}

	err = i.ensureSecret(ctx, clientId, clientSecret)
	if err != nil {
		return fmt.Errorf("unable to ensure secret: %w", err)
	}

	err = i.ensureActionsTargetSecret(ctx, targetID, signingKey)
	if err != nil {
		return fmt.Errorf("unable to ensure actions target secret: %w", err)
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
				ProviderOptions: &idp.Options{
					IsAutoCreation: g.IsAutoCreate,
					IsAutoUpdate:   g.IsAutoUpdate,
				},
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
				ProviderOptions: &idp.Options{
					IsAutoCreation: g.IsAutoCreate,
					IsAutoUpdate:   g.IsAutoUpdate,
				},
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
	redirectURIs := i.zitadelConfig.Application.RedirectUris
	if i.zitadelConfig.Application.RedirectUri != "" {
		redirectURIs = append(redirectURIs, i.zitadelConfig.Application.RedirectUri)
	}

	resp, err := i.zitadelClient.ApplicationServiceV2().CreateApplication(ctx, &app.CreateApplicationRequest{
		ProjectId:     i.zitadelConfig.Project.Id,
		Name:          i.zitadelConfig.Application.Name,
		ApplicationId: i.zitadelConfig.Application.Id,
		ApplicationType: &app.CreateApplicationRequest_OidcConfiguration{
			OidcConfiguration: &app.CreateOIDCApplicationRequest{
				RedirectUris: redirectURIs,
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
		if status.Code(err) != codes.AlreadyExists && status.Code(err) != codes.FailedPrecondition {
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
					RedirectUris: redirectURIs,
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

	_, err = i.zitadelClient.ApplicationServiceV2().UpdateApplication(ctx, &app.UpdateApplicationRequest{
		ApplicationId: i.zitadelConfig.Application.Id,
		ProjectId:     i.zitadelConfig.Project.Id,
		ApplicationType: &app.UpdateApplicationRequest_OidcConfiguration{
			OidcConfiguration: &app.UpdateOIDCApplicationConfigurationRequest{
				// adds more information to the issued token
				IdTokenUserinfoAssertion: new(true),
			},
		},
	})
	if err != nil {
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "No changes") {
			return "", "", fmt.Errorf("unable to update application: %w", err)
		}
	}

	return clientId, clientSecret, nil
}

func (i *initRunner) ensureActionsTarget(ctx context.Context) (targetID, signingKey string, err error) {
	if i.zitadelConfig.ActionsTarget.Name == "" {
		i.log.Info("no actions target configured, skipping")
		return "", "", nil
	}

	i.log.Info("ensuring actions target", "name", i.zitadelConfig.ActionsTarget.Name)

	resp, err := i.zitadelClient.ActionServiceV2().ListTargets(ctx, &action.ListTargetsRequest{
		Filters: []*action.TargetSearchFilter{
			{
				Filter: &action.TargetSearchFilter_TargetNameFilter{
					TargetNameFilter: &action.TargetNameFilter{
						TargetName: i.zitadelConfig.ActionsTarget.Name,
					},
				},
			},
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("unable to list actions targets: %w", err)
	}

	switch len(resp.Targets) {
	case 0:
		i.log.Info("creating actions target", "endpoint", i.zitadelConfig.ActionsTarget.Endpoint)

		createResp, err := i.zitadelClient.ActionServiceV2().CreateTarget(ctx, &action.CreateTargetRequest{
			Name: i.zitadelConfig.ActionsTarget.Name,
			TargetType: &action.CreateTargetRequest_RestCall{
				RestCall: &action.RESTCall{
					InterruptOnError: true,
				},
			},
			Timeout:  durationpb.New(10 * time.Second),
			Endpoint: i.zitadelConfig.ActionsTarget.Endpoint,
		})
		if err != nil {
			return "", "", fmt.Errorf("unable to create actions target: %w", err)
		}

		i.log.Info("successfully created actions target", "target-id", createResp.Id)

		return createResp.Id, createResp.SigningKey, nil
	case 1:
		i.log.Info("actions target already exists, using existing signing key")

		return resp.Targets[0].Id, resp.Targets[0].SigningKey, nil
	default:
		return "", "", fmt.Errorf("multiple actions targets already exist for name %s", i.zitadelConfig.ActionsTarget.Name)
	}
}

func (i *initRunner) ensureActionsExecution(ctx context.Context, targetID string) error {
	if targetID == "" {
		i.log.Info("no actions target configured, skipping execution")
		return nil
	}

	function := i.zitadelConfig.ActionsTarget.Function
	if function == "" {
		function = "preuserinfo"
	}

	i.log.Info("ensuring actions execution", "function", function, "target-id", targetID)

	_, err := i.zitadelClient.ActionServiceV2().SetExecution(ctx, &action.SetExecutionRequest{
		Condition: &action.Condition{
			ConditionType: &action.Condition_Function{
				Function: &action.FunctionExecution{Name: function},
			},
		},
		Targets: []string{targetID},
	})
	if err != nil {
		return fmt.Errorf("unable to set actions execution for function %s: %w", function, err)
	}

	i.log.Info("successfully ensured actions execution", "function", function)

	return nil
}

func (i *initRunner) ensureSecret(ctx context.Context, clientId, clientSecret string) error {
	var (
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      i.cfg.secretName,
				Namespace: i.cfg.namespace,
			},
		}

		regenerateSecret = func() (string, error) {
			resp, err := i.zitadelClient.ApplicationServiceV2().GenerateClientSecret(ctx, &app.GenerateClientSecretRequest{
				ProjectId:     i.zitadelConfig.Project.Id,
				ApplicationId: i.zitadelConfig.Application.Id,
			})
			if err != nil {
				return "", fmt.Errorf("unable to regenerate client secret: %w", err)
			}

			return resp.ClientSecret, nil
		}
	)

	_, err := controllerutil.CreateOrUpdate(ctx, i.kclient, secret, func() error {
		secret.Type = corev1.SecretTypeOpaque

		if clientSecret != "" {
			secret.StringData = map[string]string{
				"client_id":     clientId,
				"client_secret": clientSecret,
			}

			return nil
		}

		switch {
		case secret.Data == nil:
			break
		case len(secret.Data["client_id"]) == 0 || len(secret.Data["client_secret"]) == 0:
			break
		default:
			i.log.Info("client secret already populated, no need for regeneration")
			return nil
		}

		i.log.Info("regenerating client secret")

		clientSecret, err := regenerateSecret()
		if err != nil {
			return err
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

// ensureActionsTargetSecret writes the actions target id and signing key into a
// dedicated secret. The id and key belong together; if only one of them is set
// the caller made an error and we surface it instead of writing partial data.
func (i *initRunner) ensureActionsTargetSecret(ctx context.Context, targetID, signingKey string) error {
	if targetID == "" && signingKey == "" {
		i.log.Info("no actions target configured, skipping actions secret")
		return nil
	}

	if targetID == "" || signingKey == "" {
		return fmt.Errorf("actions target partially configured: target_id=%q signing_key=%q", targetID, signingKey)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      i.cfg.actionsSecretName,
			Namespace: i.cfg.namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, i.kclient, secret, func() error {
		secret.Type = corev1.SecretTypeOpaque
		secret.StringData = map[string]string{
			"target_id":   targetID,
			"signing_key": signingKey,
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("unable to save actions target in secret: %w", err)
	}

	return nil
}
