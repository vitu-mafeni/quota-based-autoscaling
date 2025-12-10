package restapi

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	gitea "code.gitea.io/sdk/gitea"
	"github.com/go-logr/logr"
	scalingv1 "github.com/vitumafeni/quota-based-scaling/api/v1"
	"sigs.k8s.io/yaml"
)

type ArgoAppSkipResourcesIgnoreDifferences struct {
	Group        string   `json:"group,omitempty"`
	Kind         string   `json:"kind,omitempty"`
	Name         string   `json:"name,omitempty"`
	JsonPointers []string `json:"jsonPointers,omitempty"`
}

func LogRepositories(log logr.Logger, repos []*gitea.Repository) {
	for _, repo := range repos {
		log.Info("Found repository", "name", repo.Name, "full_name", repo.FullName, "url", repo.CloneURL)
	}
}
func CreateAndPushNamespaceQuotaCR(
	ctx context.Context,
	client *gitea.Client,
	username, repoName, folder string,
	namespacequota *scalingv1.NamespaceQuota,

) (string, error) {
	log := logr.FromContextOrDiscard(ctx)

	log.Info("Pushing NamespaceQuota Resource to Gitea", "repo", repoName, "folder", folder)

	yamlData, err := yaml.Marshal(namespacequota)
	if err != nil {
		return "", fmt.Errorf("failed to marshal NamespaceQuota YAML: %w", err)
	}

	timestamp := time.Now().Format("20060102-150405")
	filename := fmt.Sprintf("%s/namespace-quota-%s.yaml", folder, timestamp)
	message := fmt.Sprintf("NamespaceQuota CR: %s/%s", namespacequota.Namespace, namespacequota.Name)

	encodedContent := base64.StdEncoding.EncodeToString(yamlData)

	fileOpts := gitea.CreateFileOptions{
		Content: encodedContent,
		FileOptions: gitea.FileOptions{
			Message:    message,
			BranchName: "main",
		},
	}

	_, _, err = client.CreateFile(username, repoName, filename, fileOpts)
	if err != nil {
		return "", fmt.Errorf("failed to create file in Gitea: %w", err)
	}

	log.Info("Successfully pushed NamespaceQuota Resource", "repo", repoName, "file", filename)
	return filename, nil
}
