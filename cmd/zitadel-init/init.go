package main

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"
	"github.com/zitadel/zitadel-go/v3/pkg/client"
	app "github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/app/v2beta"
	project "github.com/zitadel/zitadel-go/v3/pkg/client/zitadel/project/v2beta"
	"github.com/zitadel/zitadel-go/v3/pkg/zitadel"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func runInit(ctx context.Context, cmd *cli.Command) error {
	domain := cmd.String(zitadelEndpoint.Name)
	token := cmd.String(zitadelPAT.Name)
	port := cmd.Uint16(zitadelPort.Name)
	namespace := cmd.String(secretNamespace.Name)

	authOption := client.PAT(token)

	api, err := client.New(ctx, zitadel.New(domain, zitadel.WithPort(port), zitadel.WithInsecureSkipVerifyTLS()), client.WithAuth(authOption))
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

	fmt.Printf("successfully created application: %s", resp.AppId)

	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("unable to get in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("unable to create kubernetes clientset: %w", err)
	}

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "zitadel-client-credentials",
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "zitadel-init-job",
			},
		},
		StringData: map[string]string{
			"client_id":     resp.GetApiResponse().ClientId,
			"client_secret": resp.GetApiResponse().ClientSecret,
		},
		Type: v1.SecretTypeOpaque,
	}

	_, err = clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("unable to save credentials in secret: %w", err)
	}

	fmt.Printf("sucessfully created zitadel-client-credentials")

	return nil
}
