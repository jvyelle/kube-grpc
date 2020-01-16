package kubegrpc

import (
	"errors"
	"log"
	"math/rand"
	"strings"
	"time"

	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	typev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

// GrpcKubeBalancer - defines the interface required to setup the actual grpc connection
type GrpcKubeBalancer interface {
	NewGrpcClient(conn *grpc.ClientConn) (interface{}, error)
	Ping(grpcConnection interface{}) error
}

type connection struct {
	nConnections   int32 // The number of connections
	functions      GrpcKubeBalancer
	grpcConnection []*grpcConnection
}

type grpcConnection struct {
	grpcConnection interface{}
	connectionIP   string
	serviceName    string
	namespace      string
}

var (
	clientset       *kubernetes.Clientset
	connectionCache = make(map[string]*connection)
)

func init() {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("ERROR: init(): Could not get kube config in cluster. Error:" + err.Error())
	}
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("ERROR: init(): Could not connect to kube cluster with config. Error:" + err.Error())
	}
	poolManager()
}

func main() {
	// List ips from service to connect to:
	// serviceName := "elasticsearch-data"
	// svc, _ := getService(serviceName, "inca", clientset.CoreV1())
	// pods, _ := getPodsForSvc(svc, "inca", clientset.CoreV1())
	// buildConnections(serviceName, pods)
}

// poolManager - Updates the existing connection pools, keeps the pools healthy
// Runs once per second in which it pings existing connections.
// If a connection has failed, the connection is removed from the pool and a scan is executed for new connections.
// Every 60 seconds a full scan is done to check for new pods which might have been scaled into the pool
func poolManager() {
	go healthCheck()
	go updatePool()
}

// healthCheck - Runs once per second in which it pings existing connections.
// If a connection has failed, the connection is removed from the pool and a scan is executed for new connections.
// Currently there is no
func healthCheck() {
	time.Sleep(time.Second)
	// To prevent conflicts in the loops checking the connections, we use a channel without a listener active
	dirtyConnections := make(chan *grpcConnection)
	// The connections are a global variable
	for _, v := range connectionCache {
		// Iterate over the connections while calling the provided ping function
		for _, c := range v.grpcConnection {
			err := v.functions.Ping(c.grpcConnection)
			if err != nil {
				// Add to dirtyConnections channel:
				dirtyConnections <- c
			}
		}
	}
	// Activate the listener to process the changes
	close(dirtyConnections)
	go cleanConnections(dirtyConnections)
}

// cleanConnections - Processes the connections which are stale/can not be reached and removes them from the cache
func cleanConnections(dirtyConnections chan *grpcConnection) {
	// Not using a channel for this since we want unique services to be updated only (And the map deduplicates the list automatically
	updateList := make(map[string]*connection)
	for v := range dirtyConnections {
		conns := connectionCache[v.serviceName]
		// Add connection to list so that it can be used to update the existing pools
		updateList[v.serviceName] = conns
		for k, gc := range conns.grpcConnection {
			if gc == v {
				// Remove connection from slice of connections
				a := conns.grpcConnection
				conns.nConnections = conns.nConnections - 1
				a[k] = a[len(a)-1]
				a = a[:len(a)-1]
				// Value found, so no need (and very unwanted) to continue iteration since we effectively changed the iterator of the for inner for loop
				break
			}
		}
	}
	// Found connections in channel?
	for k, v := range updateList {
		updateConnectionPool(k, v.grpcConnection[0].namespace, v.functions, v)
	}
}

// updatePool - Every minute a full scan is done to check for new pods which might have been scaled into the pool
func updatePool() {
	time.Sleep(time.Minute)
	for k, v := range connectionCache {
		updateConnectionPool(k, v.grpcConnection[0].namespace, v.functions, v)
	}
}

// Connect - Call to get a connection to the given service and namespace. Will initialize a connection if not yet initialized
func Connect(serviceName, namespace string, f GrpcKubeBalancer) (interface{}, error) {
	currentCache := connectionCache[serviceName]
	if currentCache == nil {
		conn := getConnectionPool(serviceName, namespace, f)
		grcpConn := conn.grpcConnection[rand.Int31n(conn.nConnections)]
		return grcpConn, nil
	}
	return nil, nil
}

// getConnectionPool - Builds up a connection pool, initializes pool when absent, returns a pool
func getConnectionPool(serviceName, namespace string, f GrpcKubeBalancer) *connection {
	currentCache := connectionCache[serviceName]
	if currentCache == nil {
		c := &connection{
			nConnections:   0,
			functions:      f,
			grpcConnection: make([]*grpcConnection, 0),
		}
		connectionCache[serviceName] = c
		currentCache = connectionCache[serviceName]
		return updateConnectionPool(serviceName, namespace, f, currentCache)
	}
	return currentCache
}

func updateConnectionPool(serviceName, namespace string, f GrpcKubeBalancer, currentCache *connection) *connection {
	svc, _ := getService(serviceName, namespace, clientset.CoreV1())
	pods, _ := getPodsForSvc(svc, namespace, clientset.CoreV1())
	for _, pod := range pods.Items {
		// Check pool for  presense of podIP to prevent duplicate connections:
		for _, p := range currentCache.grpcConnection {
			if p.connectionIP == pod.Status.PodIP {
				// Ip found, connection alreay present, continue with the next pod:
				continue
			}
		}
		conn, err := grpc.Dial(pod.Status.PodIP, grpc.WithInsecure())
		grpcConn, err := currentCache.functions.NewGrpcClient(conn)
		if err != nil {
			// Connection could not be made, so abort, but still try next pods in list
			continue
		}
		// add to connection cache
		gc := &grpcConnection{
			connectionIP:   pod.Status.PodIP,
			grpcConnection: grpcConn,
			namespace:      namespace,   // Added to make use of channel for cleaning up connections easier
			serviceName:    serviceName, // Added to make use of channel for cleaning up connections easier
		}
		currentCache.nConnections = currentCache.nConnections + 1
		currentCache.grpcConnection = append(currentCache.grpcConnection, gc)
	}
	return currentCache
}

func getService(serviceName string, namespace string, k8sClient typev1.CoreV1Interface) (*corev1.Service, error) {
	listOptions := metav1.ListOptions{}
	svcs, err := k8sClient.Services(namespace).List(listOptions)
	if err != nil {
		log.Fatal(err)
	}
	for _, svc := range svcs.Items {
		if strings.Contains(svc.Name, serviceName) {
			return &svc, nil
		}
	}
	return nil, errors.New("cannot find service")
}

func getPodsForSvc(svc *corev1.Service, namespace string, k8sClient typev1.CoreV1Interface) (*corev1.PodList, error) {
	set := labels.Set(svc.Spec.Selector)
	listOptions := metav1.ListOptions{LabelSelector: set.AsSelector().String()}
	pods, err := k8sClient.Pods(namespace).List(listOptions)
	return pods, err
}
