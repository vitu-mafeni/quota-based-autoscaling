package restapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	// "github.com/vitu1234/quota-based-scaling/internal/controller"

	"github.com/nephio-project/nephio/controllers/pkg/resource"
	scalingv1 "github.com/vitumafeni/quota-based-scaling/api/v1"
	git "github.com/vitumafeni/quota-based-scaling/reconcilers/git"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// Global scheme for REST API server
var apiScheme = runtime.NewScheme()

func init() {
	_ = corev1.AddToScheme(apiScheme)
	_ = scalingv1.AddToScheme(apiScheme)
	metav1.AddToGroupVersion(apiScheme, schema.GroupVersion{Group: "", Version: "v1"})
}
func yamlDecode(body []byte) (runtime.Object, *schema.GroupVersionKind, error) {
	decoder := serializer.NewCodecFactory(apiScheme).UniversalDeserializer()
	return decoder.Decode(body, nil, nil)
}

// Payload sent by agents
type HeartbeatPayload struct {
	NodeName   string            `json:"node"`
	IPAddress  string            `json:"ip"`
	OS         string            `json:"os"`
	Arch       string            `json:"arch"`
	CPUCount   int               `json:"cpu"`
	MemoryMB   uint64            `json:"memory_mb"`
	Extra      map[string]string `json:"extra,omitempty"`
	ReceivedAt time.Time         `json:"received_at"`

	// From node annotations
	ClusterName string `json:"cluster_name,omitempty"`
	ClusterNS   string `json:"cluster_namespace,omitempty"`
	Machine     string `json:"machine,omitempty"`
	OwnerKind   string `json:"owner_kind,omitempty"`
	OwnerName   string `json:"owner_name,omitempty"`
	ProvidedIP  string `json:"provided_node_ip,omitempty"`
	CRISocket   string `json:"cri_socket,omitempty"`

	MissCount int `json:"-"`
}

// In-memory state store
type HeartbeatStore struct {
	sync.RWMutex
	data map[string]HeartbeatPayload
}

func NewHeartbeatStore() *HeartbeatStore {
	return &HeartbeatStore{data: make(map[string]HeartbeatPayload)}
}

func (s *HeartbeatStore) Update(hb HeartbeatPayload) {
	s.Lock()
	defer s.Unlock()
	hb.ReceivedAt = time.Now()
	hb.MissCount = 0 //reset miss counter when heartbeat is received
	s.data[hb.NodeName] = hb
}

func (s *HeartbeatStore) Get(node string) (HeartbeatPayload, bool) {
	s.RLock()
	defer s.RUnlock()
	hb, ok := s.data[node]
	return hb, ok
}

func (s *HeartbeatStore) All() []HeartbeatPayload {
	s.RLock()
	defer s.RUnlock()
	res := []HeartbeatPayload{}
	for _, hb := range s.data {
		res = append(res, hb)
	}
	return res
}

// Node fault detection threshold
// var NodeTimeout = getenvDuration("HEARTBEAT_FAULT_DELAY", 20*time.Second)

// RunQuotaBasedScalingServer starts HTTP server inside your controller
func RunQuotaBasedScalingServer(store *HeartbeatStore, addr string, Mgmtk8sClient ctrl.Client) {
	mux := http.NewServeMux()
	log := logf.FromContext(context.Background())

	// endpoint to receive heartbeats
	mux.HandleFunc("/quotabased-scaling", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		defer r.Body.Close()

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		fmt.Println("RAW BODY:")
		fmt.Println(string(body))

		// Universal Kubernetes decoder
		obj, gvk, err := yamlDecode(body)
		if err != nil {
			fmt.Printf("Decode error: %v\n", err)
			http.Error(w, "cannot decode", http.StatusBadRequest)
			return
		}

		fmt.Printf("Received: %s / %s\n", gvk.Group, gvk.Kind)

		fmt.Printf("Decoded: apiVersion=%s, kind=%s\n",
			gvk.GroupVersion().String(), gvk.Kind)

		switch o := obj.(type) {

		case *corev1.ResourceQuota:
			fmt.Printf("Parsed ResourceQuota: %s/%s\n", o.Namespace, o.Name)
			handleRQFromCluster(Mgmtk8sClient, *o)

		case *scalingv1.NamespaceQuota:
			fmt.Printf("Parsed NamespaceQuota: %s/%s\n", o.Namespace, o.Name)
			handleNamespaceQuota(Mgmtk8sClient, *o)

		default:
			fmt.Printf("Unknown object type: %T\n", o)
		}

		w.WriteHeader(http.StatusNoContent)
	})

	// optional: endpoint to list all nodes
	mux.HandleFunc("/quota-based-scaling", func(w http.ResponseWriter, r *http.Request) {
		nodes := store.All()
		w.Header().Set("Content-Type", "application/yaml")
		json.NewEncoder(w).Encode(nodes)
	})

	go func() {
		log.Info("RunQuotaBasedScalingServer server listening",
			"addr", addr,
		)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Error(err, "RunQuotaBasedScalingServer server error")
		}
	}()
}

// handle ResourceQuota from workload cluster, create or update NamespaceQuota CR
func handleRQFromCluster(Mgmtk8sClient ctrl.Client, rq corev1.ResourceQuota) {
	ctx := context.Background()
	log := logf.FromContext(ctx)

	fmt.Printf("Handling ResourceQuota from workload cluster: %s/%s\n", rq.Namespace, rq.Name)

	nqName := rq.Name // assuming same name
	nqNamespace := rq.Namespace

	// create namespace if not exists
	ns := &corev1.Namespace{}
	err := Mgmtk8sClient.Get(ctx, ctrl.ObjectKey{Name: nqNamespace}, ns)
	if err != nil {
		// create new namespace
		ns = &corev1.Namespace{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Namespace",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: nqNamespace,
			},
		}
		if err := Mgmtk8sClient.Create(ctx, ns); err != nil {
			log.Error(err, "failed to create Namespace", "name", nqNamespace)
			return
		}
		log.Info("Created Namespace for ResourceQuota from workload cluster", "name", nqNamespace)
	}

	nq := &corev1.ResourceQuota{}
	err = Mgmtk8sClient.Get(ctx, ctrl.ObjectKey{Namespace: nqNamespace, Name: nqName}, nq)
	if err != nil {
		// create new
		nq = &corev1.ResourceQuota{
			TypeMeta: metav1.TypeMeta{
				Kind:       "ResourceQuota",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Namespace: nqNamespace,
				Name:      nqName,
			},
			Spec: corev1.ResourceQuotaSpec{
				Hard: rq.Spec.Hard,
			},
		}
		if err := Mgmtk8sClient.Create(ctx, nq); err != nil {
			log.Error(err, "failed to create ResourceQuota", "name", nqName)
			return
		}
		log.Info("Created ResourceQuota from ResourceQuota from workload cluster", "name", nqName)
		return
	}

	// update existing
	nq.Spec.Hard = rq.Spec.Hard
	if err := Mgmtk8sClient.Update(ctx, nq); err != nil {
		log.Error(err, "failed to update ResourceQuota from workload cluster", "name", nqName)
		return
	}
	log.Info("Updated ResourceQuota from ResourceQuota from workload cluster", "name", nqName)
}

func handleNamespaceQuota(Mgmtk8sClient ctrl.Client, nqCR scalingv1.NamespaceQuota) {
	ctx := context.Background()
	log := logf.FromContext(ctx)

	nqName := nqCR.Name // assuming same name
	nqNamespace := nqCR.Namespace

	nsQuotaObject := &scalingv1.NamespaceQuota{}
	nsQuotaObject.TypeMeta = metav1.TypeMeta{
		Kind:       "NamespaceQuota",
		APIVersion: "scaling.dcn.ssu.ac.kr/v1",
	}
	nsQuotaObject.ObjectMeta = nqCR.ObjectMeta
	nsQuotaObject.Spec = nqCR.Spec
	nsQuotaObject.Status = nqCR.Status

	// create namespace if not exists
	ns := &corev1.Namespace{}
	err := Mgmtk8sClient.Get(ctx, ctrl.ObjectKey{Name: nqNamespace}, ns)
	if err != nil {
		// create new namespace
		ns = &corev1.Namespace{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Namespace",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: nqNamespace,
			},
		}
		if err := Mgmtk8sClient.Create(ctx, ns); err != nil {
			log.Error(err, "failed to create Namespace", "name", nqNamespace)
			return
		}
		log.Info("Created Namespace for NamespaceQuota from workload cluster", "name", nqNamespace)
	}

	nq := &scalingv1.NamespaceQuota{}
	err = Mgmtk8sClient.Get(ctx, ctrl.ObjectKey{Namespace: nqNamespace, Name: nqName}, nq)
	if err != nil {
		// create new
		nq = &scalingv1.NamespaceQuota{
			TypeMeta: metav1.TypeMeta{
				Kind:       "NamespaceQuota",
				APIVersion: "scaling.dcn.ssu.ac.kr/v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Namespace: nqNamespace,
				Name:      nqName,
			},
			Spec: scalingv1.NamespaceQuotaSpec{
				ClusterRef: nqCR.Spec.ClusterRef,
				Behavior:   nq.Spec.Behavior,
			},
		}
		if err := Mgmtk8sClient.Create(ctx, nq); err != nil {
			log.Error(err, "failed to create NamespaceQuota CR from workload cluster", "name", nqName)
			return
		}
		log.Info("Created NamespaceQuota from NamespaceQuota from workload cluster", "name", nqName)
		// return
	}

	// update existing
	nq.Spec.ClusterRef = nqCR.Spec.ClusterRef
	nq.Spec.Behavior = nqCR.Spec.Behavior
	if err := Mgmtk8sClient.Update(ctx, nq); err != nil {
		log.Error(err, "failed to update NamespaceQuota CR from workload cluster", "name", nqName)
		return
	}

	username, password, _, err := git.GetGiteaSecretUserNamePassword(ctx, Mgmtk8sClient)
	if err != nil {
		log.Error(err, "Failed to get Gitea credentials")
		return
	}

	// Wrap the controller-runtime client into a resource.APIPatchingApplicator
	applier := resource.NewAPIPatchingApplicator(Mgmtk8sClient)

	giteaClient, err := git.GetClient(ctx, applier)
	if err != nil {
		log.Error(err, "Failed to initialize Gitea client")
		return
	}

	if !giteaClient.IsInitialized() {
		log.Info("Gitea client not yet initialized, retrying later")
		return
	}

	// modify cluster api resources in the repo by incrementing node count of matching manifests
	tmpDirMgmt2, matchesMgmt2, err := git.CheckRepoForMatchingClusterAPIManifestsMgmt(ctx, "main", nsQuotaObject)
	if err != nil {
		log.Error(err, "Failed to find matching cluster API  manifests in source repo", "repo", nqCR.Spec.ClusterRef.ManagementCluster.RepositoryURL)
		return
	}

	if len(matchesMgmt2) == 0 {
		log.Info("No matching cluster API manifests found in mgmt repo - canceling operation", "repo", nqCR.Spec.ClusterRef.ManagementCluster.RepositoryURL)
		return
	}

	// newReplicaCount := -1

	// Parallelize manifest updates (limit concurrency to avoid CPU exhaustion)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 2) // Limit to 2 concurrent file operations (good for 2 vCPUs)
	errChan := make(chan error, len(matchesMgmt2))

	for _, f := range matchesMgmt2 {
		wg.Add(1)
		go func(file string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if err := git.UpdateResourceReplicasMachineDeployment(file, nqCR.Spec.Behavior.NodeScaling); err != nil {
				errChan <- fmt.Errorf("failed to update machine deploymenty manifest %s: %w", file, err)
			}
		}(f)
	}
	wg.Wait()
	close(errChan)

	for e := range errChan {
		if e != nil {
			log.Error(e, "Manifest update error for scaling")
			return
		}
	}

	// Commit & push changes
	username, password, _, err = git.GetGiteaSecretUserNamePassword(ctx, Mgmtk8sClient)
	if err != nil {
		log.Error(err, "Failed to get Gitea credentials")
		return
	}
	commitMsg := fmt.Sprintf("Update node replica count for %s/%s",
		nqCR.Spec.ClusterRef.Name, nqCR.Spec.ClusterRef.Path)

	if err := git.CommitAndPush(ctx, tmpDirMgmt2, "main", nqCR.Spec.ClusterRef.ManagementCluster.RepositoryURL, username, password, commitMsg); err != nil {
		log.Error(err, "Failed to commit & push changes for mgmt repo")
		return
	}

	if err := git.TriggerArgoCDSyncWithKubeClient(Mgmtk8sClient, nqCR.Spec.ClusterRef.ManagementCluster.Name, "argocd"); err != nil {
		log.Error(err, "Failed to trigger argocd sync with kubeclient for "+nqCR.Spec.ClusterRef.ManagementCluster.Name)
		return
	}

	log.Info("Updated MachineDeployment from NamespaceQuota from workload cluster, pushed to git success", "name", nqName)
}

func getenvDuration(key string, defaultVal time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return defaultVal
}

// UpsertNodeHealth creates or updates a NodeHealth CR
func UpsertNodeHealth(ctx context.Context, c ctrl.Client, namespace string, hb HeartbeatPayload, condition string) error {
	/*
		name := hb.NodeName
		nh := &transitionv1.NodeHealth{}

		err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, nh)
		if err != nil {
			// create new
			nh = &transitionv1.NodeHealth{
				TypeMeta: metav1.TypeMeta{
					Kind:       "NodeHealth",
					APIVersion: "nephio.dev/v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      name,
				},
				Spec: transitionv1.NodeHealthSpec{
					NodeName: hb.NodeName,
				},
				Status: transitionv1.NodeHealthStatus{
					IP:        hb.IPAddress,
					OS:        hb.OS,
					Arch:      hb.Arch,
					CPUs:      hb.CPUCount,
					MemTotal:  hb.MemoryMB,
					LastSeen:  metav1.NewTime(hb.ReceivedAt),
					Condition: condition,
				},
			}
			return c.Create(ctx, nh)
		}

		// update existing
		nh.Status.IP = hb.IPAddress
		nh.Status.OS = hb.OS
		nh.Status.Arch = hb.Arch
		nh.Status.CPUs = hb.CPUCount
		nh.Status.MemTotal = hb.MemoryMB
		nh.Status.LastSeen = metav1.NewTime(hb.ReceivedAt)
		nh.Status.Condition = condition

		// update status
		if err := c.Status().Update(ctx, nh); err != nil {
			// fallback to full update
			return c.Update(ctx, nh)
		}
	*/
	return nil
}

// MonitorNodes periodically checks heartbeat timestamps to detect faults
/*
func MonitorNodes(
	store *HeartbeatStore,
	Mgmtk8sClient ctrl.Client,
	namespace string,
	clusterPolicyReconciler *controller.ClusterPolicyReconciler,
) {


		ctx := context.Background()
		log := logf.FromContext(ctx)

		checkInterval := 100 * time.Millisecond
		//timeoutWindow := 5 * time.Second // single 5s window for detection + trigger
		warmupPeriod := 5 * time.Second // skip detection for first 5s after start
		startupTime := time.Now()

		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()

		type NodeState struct {
			TLastHeartbeat time.Time
			TDetected      time.Time
			IsTriggered    bool
			MissCount      int
		}

		state := make(map[string]*NodeState)

		for range ticker.C {
			now := time.Now()

			// Skip detection during warm-up period
			if now.Sub(startupTime) < warmupPeriod {
				continue
			}

			nodes := store.All()
			for _, hb := range nodes {
				st, exists := state[hb.NodeName]
				if !exists {
					st = &NodeState{TLastHeartbeat: hb.ReceivedAt}
					state[hb.NodeName] = st
				}

				// Update last heartbeat time
				if hb.ReceivedAt.After(st.TLastHeartbeat) {
					log.Info("heartbeat received",
						"nodeName", hb.NodeName,
						"ip", hb.IPAddress,
						"lastHeartbeat", hb.ReceivedAt.Format("15:04:05.000"),
					)
					st.TLastHeartbeat = hb.ReceivedAt
					st.IsTriggered = false
					st.MissCount = 0
					st.TDetected = time.Time{} // reset detection mark
					continue
				}

				// Check silence duration
				//silence := now.Sub(st.TLastHeartbeat)
				st.MissCount++
				if st.TDetected.IsZero() {
					st.TDetected = now
				}
				//if !st.IsTriggered && silence > timeoutWindow {
				if !st.IsTriggered && st.MissCount > 12 {
					st.TDetected = time.Now()
					st.IsTriggered = true
					detectionTime := st.TDetected.Sub(st.TLastHeartbeat)

					log.Info("Node Unhealthy detected - triggering recovery",
						"nodeName", hb.NodeName,
						"ip", hb.IPAddress,
						"T_lastHeartbeat", st.TLastHeartbeat.Format("15:04:05.000"),
						"T_detected", st.TDetected.Format("15:04:05.000"),
						"DetectionTime", detectionTime.String(),
						"T_recoveryStart", st.TDetected.Format("15:04:05.000"),
					)
					machine := &capiv1beta1.Machine{}
					//  AWS machine and node naming is different so we have to check if the node belongs to AWS first
					if strings.Contains(hb.NodeName, ".compute.internal") {
						nodeName := hb.NodeName
						// get all machines and find the one with matching node name
						var machineList capiv1beta1.MachineList
						err := Mgmtk8sClient.List(ctx, &machineList, client.InNamespace(namespace))
						if err != nil {
							log.Error(err, "failed to list CAPI machines")
							return
						}

						var machineName string

						for _, m := range machineList.Items {
							// log.Info("Found machine",
							// 	"name", m.Name,
							// 	"namespace", m.Namespace,
							// 	"providerID", m.Spec.ProviderID,
							// 	"phase", m.Status.Phase)

							if m.Status.NodeRef != nil && m.Status.NodeRef.Name != "" {
								log.Info("AWS Machine has associated Node",
									"nodeName", m.Status.NodeRef.Name)

								if nodeName == m.Status.NodeRef.Name {
									machineName = m.Name
								}
							} else {
								log.Info("could not find associated aws machine for capi node: " + nodeName)
							}
						}

						err = Mgmtk8sClient.Get(ctx, types.NamespacedName{
							Namespace: namespace,
							Name:      machineName,
						}, machine)
						if err != nil {
							log.Error(err, "could not get CAPI machine with the name "+hb.NodeName)
						}
					} else {
						err := Mgmtk8sClient.Get(ctx, types.NamespacedName{
							Namespace: namespace,
							Name:      hb.NodeName,
						}, machine)
						if err != nil {
							log.Error(err, "could not get CAPI machine with the name "+hb.NodeName)
						}
					}

					clusterName := ""
					if machine != nil {
						clusterName = machine.Spec.ClusterName
					}

					if err := clusterPolicyReconciler.ReconcileClusterPoliciesForNode(
						context.Background(), hb.NodeName, clusterName,
					); err != nil {
						log.Error(err, "ClusterPolicy reconciliation error",
							"nodeName", hb.NodeName,
							"clusterName", clusterName,
						)
					}

					if err := UpsertNodeHealth(
						context.Background(), Mgmtk8sClient, namespace, hb, "Unhealthy",
					); err != nil {
						log.Error(err, "failed to update NodeHealth",
							"nodeName", hb.NodeName,
							"clusterName", hb.ClusterName,
						)
					}
				}
			}
		}


}
*/

// StartServer starts heartbeat server and fault monitoring
func StartServer(Mgmtk8sClient ctrl.Client, addr string) {
	cfg := zap.Options{
		TimeEncoder: zapcore.TimeEncoderOfLayout("2006-01-02T15:04:05.000Z07:00"), // Milliseconds
	}
	zapLog := zap.New(zap.UseFlagOptions(&cfg))
	logf.SetLogger(zapLog)

	store := NewHeartbeatStore()
	RunQuotaBasedScalingServer(store, addr, Mgmtk8sClient)
	// go MonitorNodes(store, Mgmtk8sClient, namespace, clusterPolicyReconciler)
}
