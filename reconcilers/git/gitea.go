package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	argov1alpha1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	gitplumbing "github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	scalingv1 "github.com/vitumafeni/quota-based-scaling/api/v1"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

type IPInfo struct {
	Address string
	Gateway string
}

type NadMasterInterface struct {
	MasterInterfaceHost string
}

type MatchingNadFiles struct {
	File string
	Name string
}

// CheckRepoForMatchingManifests clones a repo and searches YAML files for a name/namespace match.
// CheckRepoForMatchingManifests clones a Git repo and scans it for YAML manifests
// matching a given ResourceRef. Uses concurrent workers for faster scanning.
func CheckRepoForMatchingNamespaceQuotaManifests(
	ctx context.Context,
	repoURL string,
	branch string,
	resourceRef *scalingv1.NamespaceQuota,
) (cloneDirectory string, matchingFiles []string, err error) {

	log := logf.FromContext(ctx)
	log.Info("Cloning repo", "url", repoURL, "branch", branch)

	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "quota-based-manifests-*")
	if err != nil {
		return "", nil, err
	}

	// Clone repo shallowly
	_, err = git.PlainCloneContext(ctx, tmpDir, false, &git.CloneOptions{
		URL:           repoURL,
		ReferenceName: gitplumbing.ReferenceName("refs/heads/" + branch),
		Depth:         1,
		SingleBranch:  true,
	})
	if err != nil {
		return "", nil, fmt.Errorf("git clone failed: %w", err)
	}

	// Channels for work and results
	fileCh := make(chan string, 100)
	matchCh := make(chan string, 100)
	errCh := make(chan error, 1)

	var wg sync.WaitGroup
	workerCount := runtime.NumCPU()

	// Worker function
	worker := func() {
		defer wg.Done()
		for path := range fileCh {
			select {
			case <-ctx.Done():
				return
			default:
			}

			data, err := os.ReadFile(path)
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}

			dec := yaml.NewDecoder(bytes.NewReader(data))
			for {
				var obj struct {
					Kind     string `yaml:"kind"`
					Metadata struct {
						Name      string `yaml:"name"`
						Namespace string `yaml:"namespace"`
					} `yaml:"metadata"`
				}

				if err := dec.Decode(&obj); err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					select {
					case errCh <- err:
					default:
					}
					return
				}

				objNs := obj.Metadata.Namespace
				if objNs == "" {
					objNs = "default"
				}
				refNs := resourceRef.Namespace
				if refNs == "" {
					refNs = "default"
				}

				if obj.Kind == resourceRef.Kind &&
					obj.Metadata.Name == resourceRef.Name &&
					objNs == refNs {
					matchCh <- path
					// don't break: one file may contain multiple matches
				}
			}
		}
	}

	// Start workers
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go worker()
	}

	// Walk directory and enqueue YAML files
	go func() {
		defer close(fileCh)
		filepath.WalkDir(tmpDir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				select {
				case errCh <- walkErr:
				default:
				}
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			if ext := strings.ToLower(filepath.Ext(path)); ext == ".yaml" || ext == ".yml" {
				fileCh <- path
			}
			return nil
		})
	}()

	// Collect matches concurrently
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(matchCh)
		close(done)
	}()

	for {
		select {
		case <-ctx.Done():
			return tmpDir, nil, ctx.Err()
		case e := <-errCh:
			if e != nil {
				return tmpDir, nil, e
			}
		case path, ok := <-matchCh:
			if !ok {
				// done
				return tmpDir, matchingFiles, nil
			}
			matchingFiles = append(matchingFiles, path)
		case <-done:
			return tmpDir, matchingFiles, nil
		}
	}
}

// UpdateResourceContainers updates matching container images
// inside a local Kubernetes manifest file.
// images: map[containerName]newImage
func UpdateResourceContainers(path string, imageCheckpoint, originalImage string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	var doc map[string]interface{}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("unmarshal yaml: %w", err)
	}

	kind, _ := doc["kind"].(string)
	if !isSupportedKind(kind) {
		return fmt.Errorf("%s: unsupported Kind %q", path, kind)
	}

	spec, ok := doc["spec"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("%s: missing spec", path)
	}

	// All these workload types keep pod spec under spec.template.spec
	tpl, ok := spec["template"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("%s: missing spec.template", path)
	}
	resourceSpec, ok := tpl["spec"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("%s: missing spec.template.spec", path)
	}

	changed := updateContainers(resourceSpec, imageCheckpoint, originalImage)
	if !changed {
		return nil // nothing to do
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal yaml: %w", err)
	}

	if changed {
		fmt.Printf("Updated container image %s: %s -> %s\n", path, originalImage, imageCheckpoint)
	}

	return os.WriteFile(path, out, 0644)
}

func isSupportedKind(k string) bool {
	switch k {
	case "Deployment", "ReplicaSet", "DaemonSet", "StatefulSet":
		return true
	default:
		return false
	}
}

// updateContainers updates containers and initContainers by name.
func updateContainers(resourceSpec map[string]interface{}, imageCheckpoint, originalImage string) bool {
	changed := false
	for _, field := range []string{"containers", "initContainers"} {
		if arr, ok := resourceSpec[field].([]interface{}); ok {
			for _, c := range arr {
				if cm, ok := c.(map[string]interface{}); ok {
					if image, _ := cm["image"].(string); image != "" {
						if image == originalImage {
							cm["image"] = imageCheckpoint
							changed = true
						}
					}
				}
			}
		}
	}
	return changed
}

// CommitAndPush commits all staged changes in cloneDir and pushes to the remote branch.
// userName and password are Gitea credentials (from your secret).
func CommitAndPush(ctx context.Context, cloneDir, branch, repoURL, userName, password, commitMsg string) error {
	// userName, password, _, err := giteaclient.GetGiteaSecretUserNamePassword(ctx)

	// Open the repository
	r, err := git.PlainOpen(cloneDir)
	if err != nil {
		return fmt.Errorf("open repo: %w", err)
	}

	// Stage all changes
	w, err := r.Worktree()
	if err != nil {
		return fmt.Errorf("get worktree: %w", err)
	}
	if err := w.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return fmt.Errorf("git add: %w", err)
	}

	// Create commit
	_, err = w.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{
			Name:  userName,
			Email: fmt.Sprintf("%s@example.com", userName),
			When:  time.Now(),
		},
	})
	if err != nil {
		return fmt.Errorf("git commit: %w", err)
	}

	// Push commit
	if err := r.Push(&git.PushOptions{
		RemoteName: "origin",
		RefSpecs: []config.RefSpec{
			config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", branch, branch)),
		},
		Auth: &http.BasicAuth{
			Username: userName,
			Password: password,
		},
	}); err != nil {
		return fmt.Errorf("git push: %w", err)
	}

	return nil
}

// argocd sync | below is original
func TriggerArgoCDSyncWithKubeClient(k8sClient client.Client, appName, namespace string) error {
	ctx := context.TODO()

	// fmt.Printf("Triggering ArgoCD sync for application %q in namespace %q\n", appName, namespace)

	// Get the current Application object
	var app argov1alpha1.Application
	err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      appName,
		Namespace: namespace,
	}, &app)
	if err != nil {
		return fmt.Errorf("failed to get Argo CD application: %w", err)
	}

	// Deep copy to modify
	// updated := app.DeepCopy()
	// now := time.Now().UTC()

	// Update Operation field to trigger sync
	app.Operation = &argov1alpha1.Operation{
		Sync: &argov1alpha1.SyncOperation{
			Revision: "HEAD",
			SyncStrategy: &argov1alpha1.SyncStrategy{
				Apply: &argov1alpha1.SyncStrategyApply{
					Force: true, // This enables kubectl apply --force
				},
			},
		},
		InitiatedBy: argov1alpha1.OperationInitiator{
			Username: "gitea-client",
		},
		// StartedAt: &now,
	}

	//doing operations for this argocd application
	// fmt.Printf("Triggering sync for application %v", app)
	// fmt.Printf("Triggering sync for application %s in namespace %s\n", appName, namespace)

	if err := k8sClient.Update(ctx, &app); err != nil {
		return fmt.Errorf("failed to update application with sync operation: %w", err)
	}

	return nil
}

// listArgoApplications lists all Argo CD Applications in the argocd namespace
func ListArgoApplications(ctx context.Context, k8sClient client.Client) error {
	log := logf.FromContext(ctx)
	var appList argov1alpha1.ApplicationList
	if err := k8sClient.List(
		ctx,
		&appList,
		&client.ListOptions{Namespace: "argocd"},
	); err != nil {
		return err
	}

	for _, app := range appList.Items {
		log.Info("Found Argo CD Application",
			"name", app.Name,
			"project", app.Spec.Project,
			"destNamespace", app.Spec.Destination.Namespace,
			"destServer", app.Spec.Destination.Server,
		)
	}
	return nil
}

//create new argo app and push to the target cluster
