/*
Copyright © 2020 The k3d Author(s)

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/
package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	gort "runtime"

	"github.com/docker/go-connections/nat"
	"github.com/imdario/mergo"
	"github.com/rancher/k3d/v4/pkg/actions"
	config "github.com/rancher/k3d/v4/pkg/config/v1alpha1"
	k3drt "github.com/rancher/k3d/v4/pkg/runtimes"
	"github.com/rancher/k3d/v4/pkg/runtimes/docker"
	runtimeErr "github.com/rancher/k3d/v4/pkg/runtimes/errors"
	"github.com/rancher/k3d/v4/pkg/types"
	k3d "github.com/rancher/k3d/v4/pkg/types"
	"github.com/rancher/k3d/v4/pkg/util"
	"github.com/rancher/k3d/v4/version"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

// ClusterRun orchestrates the steps of cluster creation, configuration and starting
func ClusterRun(ctx context.Context, runtime k3drt.Runtime, clusterConfig *config.ClusterConfig) error {
	/*
	 * Step 0: (Infrastructure) Preparation
	 */
	if err := ClusterPrep(ctx, runtime, clusterConfig); err != nil {
		return fmt.Errorf("Failed Cluster Preparation: %+v", err)
	}

	/*
	 * Step 1: Create Containers
	 */
	if err := ClusterCreate(ctx, runtime, &clusterConfig.Cluster, &clusterConfig.ClusterCreateOpts); err != nil {
		return fmt.Errorf("Failed Cluster Creation: %+v", err)
	}

	/*
	 * Step 2: Pre-Start Configuration
	 */
	// TODO: ClusterRun: add cluster configuration step here

	/*
	 * Step 3: Start Containers
	 */
	if err := ClusterStart(ctx, runtime, &clusterConfig.Cluster, k3d.ClusterStartOpts{
		WaitForServer: clusterConfig.ClusterCreateOpts.WaitForServer,
		Timeout:       clusterConfig.ClusterCreateOpts.Timeout, // TODO: here we should consider the time used so far
		NodeHooks:     clusterConfig.ClusterCreateOpts.NodeHooks,
	}); err != nil {
		return fmt.Errorf("Failed Cluster Start: %+v", err)
	}

	/*
	 * Post-Start Configuration
	 */
	/**********************************
	 * Additional Cluster Preparation *
	 **********************************/

	/*
	 * Networking Magic
	 */

	// add /etc/hosts and CoreDNS entry for host.k3d.internal, referring to the host system
	if !clusterConfig.ClusterCreateOpts.PrepDisableHostIPInjection {
		prepInjectHostIP(ctx, runtime, &clusterConfig.Cluster)
	}

	// create the registry hosting configmap
	if err := prepCreateLocalRegistryHostingConfigMap(ctx, runtime, &clusterConfig.Cluster); err != nil {
		log.Warnf("Failed to create LocalRegistryHosting ConfigMap: %+v", err)
	}

	return nil
}

// ClusterPrep takes care of the steps required before creating/starting the cluster containers
func ClusterPrep(ctx context.Context, runtime k3drt.Runtime, clusterConfig *config.ClusterConfig) error {
	/*
	 * Set up contexts
	 * Used for (early) termination (across API boundaries)
	 */
	clusterPrepCtx := ctx
	if clusterConfig.ClusterCreateOpts.Timeout > 0*time.Second {
		var cancelClusterPrepCtx context.CancelFunc
		clusterPrepCtx, cancelClusterPrepCtx = context.WithTimeout(ctx, clusterConfig.ClusterCreateOpts.Timeout)
		defer cancelClusterPrepCtx()
	}

	/*
	 * Step 0: Pre-Pull Images
	 */
	// TODO: ClusterPrep: add image pre-pulling step

	/*
	 * Step 1: Network
	 */
	if err := ClusterPrepNetwork(clusterPrepCtx, runtime, &clusterConfig.Cluster, &clusterConfig.ClusterCreateOpts); err != nil {
		return fmt.Errorf("Failed Network Preparation: %+v", err)
	}

	/*
	 * Step 2: Volume(s)
	 */
	if !clusterConfig.ClusterCreateOpts.DisableImageVolume {
		if err := ClusterPrepImageVolume(ctx, runtime, &clusterConfig.Cluster, &clusterConfig.ClusterCreateOpts); err != nil {
			return fmt.Errorf("Failed Image Volume Preparation: %+v", err)
		}
	}

	/*
	 * Step 3: Registries
	 */

	// Ensure referenced registries
	for _, reg := range clusterConfig.ClusterCreateOpts.Registries.Use {
		log.Debugf("Trying to find registry %s", reg.Host)
		regNode, err := runtime.GetNode(ctx, &k3d.Node{Name: reg.Host})
		if err != nil {
			return fmt.Errorf("Failed to find registry node '%s': %+v", reg.Host, err)
		}
		regFromNode, err := RegistryFromNode(regNode)
		if err != nil {
			return err
		}
		*reg = *regFromNode
	}

	// Create managed registry bound to this cluster
	if clusterConfig.ClusterCreateOpts.Registries.Create != nil {
		registryNode, err := RegistryCreate(ctx, runtime, clusterConfig.ClusterCreateOpts.Registries.Create)
		if err != nil {
			return fmt.Errorf("Failed to create registry: %+v", err)
		}

		clusterConfig.Cluster.Nodes = append(clusterConfig.Cluster.Nodes, registryNode)

		clusterConfig.ClusterCreateOpts.Registries.Use = append(clusterConfig.ClusterCreateOpts.Registries.Use, clusterConfig.ClusterCreateOpts.Registries.Create)
	}

	// Use existing registries (including the new one, if created)
	log.Tracef("Using Registries: %+v", clusterConfig.ClusterCreateOpts.Registries.Use)

	if len(clusterConfig.ClusterCreateOpts.Registries.Use) > 0 {
		// ensure that all selected registries exist and connect them to the cluster network
		for _, externalReg := range clusterConfig.ClusterCreateOpts.Registries.Use {
			regNode, err := runtime.GetNode(ctx, &k3d.Node{Name: externalReg.Host})
			if err != nil {
				return fmt.Errorf("Failed to find registry node '%s': %+v", externalReg.Host, err)
			}
			if err := RegistryConnectNetworks(ctx, runtime, regNode, []string{clusterConfig.Cluster.Network.Name}); err != nil {
				return fmt.Errorf("Failed to connect registry node '%s' to cluster network: %+v", regNode.Name, err)
			}
		}

		// generate the registries.yaml
		regConf, err := RegistryGenerateK3sConfig(ctx, clusterConfig.ClusterCreateOpts.Registries.Use)
		if err != nil {
			return fmt.Errorf("Failed to generate registry config file for k3s: %+v", err)
		}
		RegistryMergeConfig(ctx, regConf, clusterConfig.ClusterCreateOpts.Registries.Config)
		log.Tracef("Merged registry config: %+v", regConf)
		regConfBytes, err := yaml.Marshal(&regConf)
		if err != nil {
			return fmt.Errorf("Failed to marshal registry configuration: %+v", err)
		}
		clusterConfig.ClusterCreateOpts.NodeHooks = append(clusterConfig.ClusterCreateOpts.NodeHooks, k3d.NodeHook{
			Stage: k3d.LifecycleStagePreStart,
			Action: actions.WriteFileAction{
				Runtime: runtime,
				Content: regConfBytes,
				Dest:    k3d.DefaultRegistriesFilePath,
			},
		})

		// generate the LocalRegistryHosting configmap
		regCm, err := RegistryGenerateLocalRegistryHostingConfigMapYAML(ctx, clusterConfig.ClusterCreateOpts.Registries.Use)
		if err != nil {
			return fmt.Errorf("Failed to generate LocalRegistryHosting configmap: %+v", err)
		}
		log.Tracef("Writing LocalRegistryHosting YAML:\n%s", string(regCm))
		clusterConfig.ClusterCreateOpts.NodeHooks = append(clusterConfig.ClusterCreateOpts.NodeHooks, k3d.NodeHook{
			Stage: k3d.LifecycleStagePreStart,
			Action: actions.WriteFileAction{
				Runtime: runtime,
				Content: regCm,
				Dest:    "/tmp/reg.yaml",
			},
		})

	}

	return nil

}

// ClusterPrepNetwork creates a new cluster network, if needed or sets everything up to re-use an existing network
func ClusterPrepNetwork(ctx context.Context, runtime k3drt.Runtime, cluster *k3d.Cluster, clusterCreateOpts *k3d.ClusterCreateOpts) error {
	log.Infoln("Prep: Network")

	// error out if external cluster network should be used but no name was set
	if cluster.Network.Name == "" && cluster.Network.External {
		return fmt.Errorf("Failed to use external network because no name was specified")
	}

	// generate cluster network name, if not set
	if cluster.Network.Name == "" && !cluster.Network.External {
		cluster.Network.Name = fmt.Sprintf("%s-%s", k3d.DefaultObjectNamePrefix, cluster.Name)
	}

	// handle hostnetwork
	if cluster.Network.Name == "host" {
		if len(cluster.Nodes) > 1 {
			return fmt.Errorf("Only one server node supported when using host network")
		}
	}

	// create cluster network or use an existing one
	networkID, networkExists, err := runtime.CreateNetworkIfNotPresent(ctx, cluster.Network.Name)
	if err != nil {
		log.Errorln("Failed to create cluster network")
		return err
	}
	cluster.Network.Name = networkID
	clusterCreateOpts.GlobalLabels[k3d.LabelNetwork] = networkID
	clusterCreateOpts.GlobalLabels[k3d.LabelNetworkExternal] = strconv.FormatBool(cluster.Network.External)
	if networkExists {
		clusterCreateOpts.GlobalLabels[k3d.LabelNetworkExternal] = "true" // if the network wasn't created, we say that it's managed externally (important for cluster deletion)
	}

	return nil
}

func ClusterPrepImageVolume(ctx context.Context, runtime k3drt.Runtime, cluster *k3d.Cluster, clusterCreateOpts *k3d.ClusterCreateOpts) error {
	/*
	 * Cluster-Wide volumes
	 * - image volume (for importing images)
	 */
	imageVolumeName := fmt.Sprintf("%s-%s-images", k3d.DefaultObjectNamePrefix, cluster.Name)
	if err := runtime.CreateVolume(ctx, imageVolumeName, map[string]string{k3d.LabelClusterName: cluster.Name}); err != nil {
		log.Errorf("Failed to create image volume '%s' for cluster '%s'", imageVolumeName, cluster.Name)
		return err
	}

	clusterCreateOpts.GlobalLabels[k3d.LabelImageVolume] = imageVolumeName

	// attach volume to nodes
	for _, node := range cluster.Nodes {
		node.Volumes = append(node.Volumes, fmt.Sprintf("%s:%s", imageVolumeName, k3d.DefaultImageVolumeMountPath))
	}
	return nil
}

// ClusterCreate creates a new cluster consisting of
// - some containerized k3s nodes
// - a docker network
func ClusterCreate(ctx context.Context, runtime k3drt.Runtime, cluster *k3d.Cluster, clusterCreateOpts *k3d.ClusterCreateOpts) error {

	log.Tracef(`
===== Creating Cluster =====

Runtime:
%+v

Cluster:
%+v

ClusterCreatOpts:
%+v

============================
	`, runtime, cluster, clusterCreateOpts)

	/*
	 * Set up contexts
	 * Used for (early) termination (across API boundaries)
	 */
	clusterCreateCtx := ctx
	if clusterCreateOpts.Timeout > 0*time.Second {
		var cancelClusterCreateCtx context.CancelFunc
		clusterCreateCtx, cancelClusterCreateCtx = context.WithTimeout(ctx, clusterCreateOpts.Timeout)
		defer cancelClusterCreateCtx()
	}

	/*
	 * Docker Machine Special Configuration
	 */
	if cluster.KubeAPI.Host == k3d.DefaultAPIHost && runtime == k3drt.Docker {
		if gort.GOOS == "windows" || gort.GOOS == "darwin" {
			log.Tracef("Running on %s: checking if it's using docker-machine", gort.GOOS)
			machineIP, err := runtime.(docker.Docker).GetDockerMachineIP()
			if err != nil {
				log.Warnf("Using docker-machine, but failed to get it's IP: %+v", err)
			} else if machineIP != "" {
				log.Infof("Using the docker-machine IP %s to connect to the Kubernetes API", machineIP)
				cluster.KubeAPI.Host = machineIP
				cluster.KubeAPI.Binding.HostIP = machineIP
			} else {
				log.Traceln("Not using docker-machine")
			}
		}
	}

	/*
	 * Cluster Token
	 */

	if cluster.Token == "" {
		cluster.Token = GenerateClusterToken()
	}
	clusterCreateOpts.GlobalLabels[k3d.LabelClusterToken] = cluster.Token

	/*
	 * Nodes
	 */

	clusterCreateOpts.GlobalLabels[k3d.LabelClusterName] = cluster.Name

	// agent defaults (per cluster)
	// connection url is always the name of the first server node (index 0)
	connectionURL := fmt.Sprintf("https://%s:%s", generateNodeName(cluster.Name, k3d.ServerRole, 0), k3d.DefaultAPIPort)
	clusterCreateOpts.GlobalLabels[k3d.LabelClusterURL] = connectionURL
	clusterCreateOpts.GlobalEnv = append(clusterCreateOpts.GlobalEnv, fmt.Sprintf("K3S_TOKEN=%s", cluster.Token))

	nodeSetup := func(node *k3d.Node, suffix int) error {
		// cluster specific settings
		if node.Labels == nil {
			node.Labels = make(map[string]string) // TODO: maybe create an init function?
		}

		// ensure global labels
		for k, v := range clusterCreateOpts.GlobalLabels {
			node.Labels[k] = v
		}

		// ensure global env
		node.Env = append(node.Env, clusterCreateOpts.GlobalEnv...)

		// node role specific settings
		if node.Role == k3d.ServerRole {

			node.ServerOpts.KubeAPI = cluster.KubeAPI

			// the cluster has an init server node, but its not this one, so connect it to the init node
			if cluster.InitNode != nil && !node.ServerOpts.IsInit {
				node.Env = append(node.Env, fmt.Sprintf("K3S_URL=%s", connectionURL))
			}

		} else if node.Role == k3d.AgentRole {
			node.Env = append(node.Env, fmt.Sprintf("K3S_URL=%s", connectionURL))
		}

		node.Name = generateNodeName(cluster.Name, node.Role, suffix)
		node.Network = cluster.Network.Name
		node.Restart = true
		node.GPURequest = clusterCreateOpts.GPURequest

		// create node
		log.Infof("Creating node '%s'", node.Name)
		if err := NodeCreate(clusterCreateCtx, runtime, node, k3d.NodeCreateOpts{}); err != nil {
			log.Errorln("Failed to create node")
			return err
		}
		log.Debugf("Created node '%s'", node.Name)

		// start node
		//return NodeStart(clusterCreateCtx, runtime, node, k3d.NodeStartOpts{PreStartActions: clusterCreateOpts.NodeHookActions})
		return nil
	}

	// used for node suffices
	serverCount := 0
	agentCount := 0
	suffix := 0

	// create init node first
	if cluster.InitNode != nil {
		log.Infoln("Creating initializing server node")
		cluster.InitNode.Args = append(cluster.InitNode.Args, "--cluster-init")

		// in case the LoadBalancer was disabled, expose the API Port on the initializing server node
		if clusterCreateOpts.DisableLoadBalancer {
			cluster.InitNode.Ports[k3d.DefaultAPIPort] = []nat.PortBinding{cluster.KubeAPI.Binding}
		}

		if err := nodeSetup(cluster.InitNode, serverCount); err != nil {
			return err
		}
		serverCount++

	}

	// create all other nodes, but skip the init node
	for _, node := range cluster.Nodes {
		if node.Role == k3d.ServerRole {

			// skip the init node here
			if node == cluster.InitNode {
				continue
			} else if serverCount == 0 && clusterCreateOpts.DisableLoadBalancer {
				// if this is the first server node and the server loadbalancer is disabled, expose the API Port on this server node
				node.Ports[k3d.DefaultAPIPort] = []nat.PortBinding{cluster.KubeAPI.Binding}
			}

			time.Sleep(1 * time.Second) // FIXME: arbitrary wait for one second to avoid race conditions of servers registering

			// name suffix
			suffix = serverCount
			serverCount++

		} else if node.Role == k3d.AgentRole {
			// name suffix
			suffix = agentCount
			agentCount++
		}
		if node.Role == k3d.ServerRole || node.Role == k3d.AgentRole {
			if err := nodeSetup(node, suffix); err != nil {
				return err
			}
		}
	}

	/*
	 * Auxiliary Containers
	 */
	// *** ServerLoadBalancer ***
	if !clusterCreateOpts.DisableLoadBalancer {
		if cluster.Network.Name != "host" { // serverlb not supported in hostnetwork mode due to port collisions with server node
			// Generate a comma-separated list of server/server names to pass to the LB container
			servers := ""
			for _, node := range cluster.Nodes {
				if node.Role == k3d.ServerRole {
					if servers == "" {
						servers = node.Name
					} else {
						servers = fmt.Sprintf("%s,%s", servers, node.Name)
					}
				}
			}

			// generate comma-separated list of extra ports to forward
			ports := k3d.DefaultAPIPort
			for exposedPort := range cluster.ServerLoadBalancer.Ports {
				ports += "," + exposedPort.Port()
			}

			if cluster.ServerLoadBalancer.Ports == nil {
				cluster.ServerLoadBalancer.Ports = nat.PortMap{}
			}
			cluster.ServerLoadBalancer.Ports[k3d.DefaultAPIPort] = []nat.PortBinding{cluster.KubeAPI.Binding}

			// Create LB as a modified node with loadbalancerRole
			lbNode := &k3d.Node{
				Name:  fmt.Sprintf("%s-%s-serverlb", k3d.DefaultObjectNamePrefix, cluster.Name),
				Image: fmt.Sprintf("%s:%s", k3d.DefaultLBImageRepo, version.GetHelperImageVersion()),
				Ports: cluster.ServerLoadBalancer.Ports,
				Env: []string{
					fmt.Sprintf("SERVERS=%s", servers),
					fmt.Sprintf("PORTS=%s", ports),
					fmt.Sprintf("WORKER_PROCESSES=%d", len(strings.Split(ports, ","))),
				},
				Role:    k3d.LoadBalancerRole,
				Labels:  clusterCreateOpts.GlobalLabels, // TODO: createLoadBalancer: add more expressive labels
				Network: cluster.Network.Name,
				Restart: true,
			}
			cluster.Nodes = append(cluster.Nodes, lbNode) // append lbNode to list of cluster nodes, so it will be considered during rollback
			log.Infof("Creating LoadBalancer '%s'", lbNode.Name)
			if err := NodeCreate(clusterCreateCtx, runtime, lbNode, k3d.NodeCreateOpts{}); err != nil {
				log.Errorln("Failed to create loadbalancer")
				return err
			}
			log.Debugf("Created loadbalancer '%s'", lbNode.Name)
		} else {
			log.Infoln("Hostnetwork selected -> Skipping creation of server LoadBalancer")
		}
	}

	return nil
}

// ClusterDelete deletes an existing cluster
func ClusterDelete(ctx context.Context, runtime k3drt.Runtime, cluster *k3d.Cluster) error {

	log.Infof("Deleting cluster '%s'", cluster.Name)
	cluster, err := ClusterGet(ctx, runtime, cluster)
	if err != nil {
		return err
	}
	log.Debugf("Cluster Details: %+v", cluster)

	failed := 0
	for _, node := range cluster.Nodes {
		if err := runtime.DeleteNode(ctx, node); err != nil {
			log.Warningf("Failed to delete node '%s': Try to delete it manually", node.Name)
			failed++
			continue
		}
	}

	// Delete the cluster network, if it was created for/by this cluster (and if it's not in use anymore)
	if cluster.Network.Name != "" {
		if !cluster.Network.External {
			log.Infof("Deleting cluster network '%s'", cluster.Network.Name)
			if err := runtime.DeleteNetwork(ctx, cluster.Network.Name); err != nil {
				if errors.Is(err, runtimeErr.ErrRuntimeNetworkNotEmpty) { // there are still containers connected to that network

					connectedNodes, err := runtime.GetNodesInNetwork(ctx, cluster.Network.Name) // check, if there are any k3d nodes connected to the cluster
					if err != nil {
						log.Warningf("Failed to check cluster network for connected nodes: %+v", err)
					}

					if len(connectedNodes) > 0 { // there are still k3d-managed containers (aka nodes) connected to the network
						connectedRegistryNodes := util.FilterNodesByRole(connectedNodes, k3d.RegistryRole)
						if len(connectedRegistryNodes) == len(connectedNodes) { // only registry node(s) left in the network
							for _, node := range connectedRegistryNodes {
								log.Debugf("Disconnecting registry node %s from the network...", node.Name)
								if err := runtime.DisconnectNodeFromNetwork(ctx, node, cluster.Network.Name); err != nil {
									log.Warnf("Failed to disconnect registry %s from network %s", node.Name, cluster.Network.Name)
								} else {
									if err := runtime.DeleteNetwork(ctx, cluster.Network.Name); err != nil {
										log.Warningf("Failed to delete cluster network, even after disconnecting registry node(s): %+v", err)
									}
								}
							}
						} else { // besides the registry node(s), there are still other nodes... maybe they still need a registry
							log.Debugf("There are some non-registry nodes left in the network")
						}
					} else {
						log.Warningf("Failed to delete cluster network '%s' because it's still in use: is there another cluster using it?", cluster.Network.Name)
					}
				} else {
					log.Warningf("Failed to delete cluster network '%s': '%+v'", cluster.Network.Name, err)
				}
			}
		} else if cluster.Network.External {
			log.Debugf("Skip deletion of cluster network '%s' because it's managed externally", cluster.Network.Name)
		}
	}

	// delete image volume
	if cluster.ImageVolume != "" {
		log.Infof("Deleting image volume '%s'", cluster.ImageVolume)
		if err := runtime.DeleteVolume(ctx, cluster.ImageVolume); err != nil {
			log.Warningf("Failed to delete image volume '%s' of cluster '%s': Try to delete it manually", cluster.ImageVolume, cluster.Name)
		}
	}

	// return error if we failed to delete a node
	if failed > 0 {
		return fmt.Errorf("Failed to delete %d nodes: Try to delete them manually", failed)
	}
	return nil
}

// ClusterList returns a list of all existing clusters
func ClusterList(ctx context.Context, runtime k3drt.Runtime) ([]*k3d.Cluster, error) {
	log.Traceln("Listing Clusters...")
	nodes, err := runtime.GetNodesByLabel(ctx, k3d.DefaultObjectLabels)
	if err != nil {
		log.Errorln("Failed to get clusters")
		return nil, err
	}

	log.Debugf("Found %d nodes", len(nodes))
	if log.GetLevel() == log.TraceLevel {
		for _, node := range nodes {
			log.Tracef("Found node %s of role %s", node.Name, node.Role)
		}
	}

	nodes = NodeFilterByRoles(nodes, k3d.ClusterInternalNodeRoles, k3d.ClusterExternalNodeRoles)

	log.Tracef("Found %d cluster-internal nodes", len(nodes))
	if log.GetLevel() == log.TraceLevel {
		for _, node := range nodes {
			log.Tracef("Found cluster-internal node %s of role %s belonging to cluster %s", node.Name, node.Role, node.Labels[k3d.LabelClusterName])
		}
	}

	clusters := []*k3d.Cluster{}
	// for each node, check, if we can add it to a cluster or add the cluster if it doesn't exist yet
	for _, node := range nodes {
		clusterExists := false
		for _, cluster := range clusters {
			if node.Labels[k3d.LabelClusterName] == cluster.Name { // TODO: handle case, where this label doesn't exist
				cluster.Nodes = append(cluster.Nodes, node)
				clusterExists = true
				break
			}
		}
		// cluster is not in the list yet, so we add it with the current node as its first member
		if !clusterExists {
			clusters = append(clusters, &k3d.Cluster{
				Name:  node.Labels[k3d.LabelClusterName],
				Nodes: []*k3d.Node{node},
			})
		}
	}

	// enrich cluster structs with label values
	for _, cluster := range clusters {
		if err := populateClusterFieldsFromLabels(cluster); err != nil {
			log.Warnf("Failed to populate cluster fields from node label values for cluster '%s'", cluster.Name)
			log.Warnln(err)
		}
	}
	log.Debugf("Found %d clusters", len(clusters))
	return clusters, nil
}

// populateClusterFieldsFromLabels inspects labels attached to nodes and translates them to struct fields
func populateClusterFieldsFromLabels(cluster *k3d.Cluster) error {
	networkExternalSet := false

	for _, node := range cluster.Nodes {

		// get the name of the cluster network
		if cluster.Network.Name == "" {
			if networkName, ok := node.Labels[k3d.LabelNetwork]; ok {
				cluster.Network.Name = networkName
			}
		}

		// check if the network is external
		// since the struct value is a bool, initialized as false, we cannot check if it's unset
		if !cluster.Network.External && !networkExternalSet {
			if networkExternalString, ok := node.Labels[k3d.LabelNetworkExternal]; ok {
				if networkExternal, err := strconv.ParseBool(networkExternalString); err == nil {
					cluster.Network.External = networkExternal
					networkExternalSet = true
				}
			}
		}

		// get image volume // TODO: enable external image volumes the same way we do it with networks
		if cluster.ImageVolume == "" {
			if imageVolumeName, ok := node.Labels[k3d.LabelImageVolume]; ok {
				cluster.ImageVolume = imageVolumeName
			}
		}

		// get k3s cluster's token
		if cluster.Token == "" {
			if token, ok := node.Labels[k3d.LabelClusterToken]; ok {
				cluster.Token = token
			}
		}
	}

	return nil
}

var ClusterGetNoNodesFoundError = errors.New("No nodes found for given cluster")

// ClusterGet returns an existing cluster with all fields and node lists populated
func ClusterGet(ctx context.Context, runtime k3drt.Runtime, cluster *k3d.Cluster) (*k3d.Cluster, error) {
	// get nodes that belong to the selected cluster
	nodes, err := runtime.GetNodesByLabel(ctx, map[string]string{k3d.LabelClusterName: cluster.Name})
	if err != nil {
		log.Errorf("Failed to get nodes for cluster '%s'", cluster.Name)
	}

	if len(nodes) == 0 {
		return nil, ClusterGetNoNodesFoundError
	}

	// append nodes
	for _, node := range nodes {

		// check if there's already a node in the struct
		overwroteExisting := false
		for _, existingNode := range cluster.Nodes {

			// overwrite existing node
			if existingNode.Name == node.Name {
				mergo.MergeWithOverwrite(existingNode, node)
				overwroteExisting = true
			}
		}

		// no existing node overwritten: append new node
		if !overwroteExisting {
			cluster.Nodes = append(cluster.Nodes, node)
		}
	}

	if err := populateClusterFieldsFromLabels(cluster); err != nil {
		log.Warnf("Failed to populate cluster fields from node labels")
		log.Warnln(err)
	}

	return cluster, nil
}

// GenerateClusterToken generates a random 20 character string
func GenerateClusterToken() string {
	return util.GenerateRandomString(20)
}

func generateNodeName(cluster string, role k3d.Role, suffix int) string {
	return fmt.Sprintf("%s-%s-%s-%d", k3d.DefaultObjectNamePrefix, cluster, role, suffix)
}

// ClusterStart starts a whole cluster (i.e. all nodes of the cluster)
func ClusterStart(ctx context.Context, runtime k3drt.Runtime, cluster *k3d.Cluster, startClusterOpts types.ClusterStartOpts) error {
	log.Infof("Starting cluster '%s'", cluster.Name)

	start := time.Now()

	if startClusterOpts.Timeout > 0*time.Second {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, startClusterOpts.Timeout)
		defer cancel()
	}

	/*
	 * Init Node
	 */
	for _, n := range cluster.Nodes {
		if n.Role == k3d.ServerRole && n.ServerOpts.IsInit {
			if err := NodeStart(ctx, runtime, n, k3d.NodeStartOpts{
				NodeHooks: startClusterOpts.NodeHooks,
			}); err != nil {
				return fmt.Errorf("Failed to start initializing server node: %+v", err)
			}
			// wait for the initnode to come up before doing anything else
			for {
				select {
				case <-ctx.Done():
					log.Errorln("Failed to bring up initializing server node in time")
					return fmt.Errorf(">>> %w", ctx.Err())
				default:
				}
				log.Debugln("Waiting for initializing server node...")
				logreader, err := runtime.GetNodeLogs(ctx, cluster.InitNode, time.Time{})
				if err != nil {
					if logreader != nil {
						logreader.Close()
					}
					log.Errorln(err)
					log.Errorln("Failed to get logs from the initializig server node.. waiting for 3 seconds instead")
					time.Sleep(3 * time.Second)
					break
				}
				defer logreader.Close()
				buf := new(bytes.Buffer)
				nRead, _ := buf.ReadFrom(logreader)
				logreader.Close()
				if nRead > 0 && strings.Contains(buf.String(), k3d.ReadyLogMessageByRole[k3d.ServerRole]) {
					log.Debugln("Initializing server node is up... continuing")
					break
				}
				time.Sleep(time.Second)
			}
			break
		}
	}

	/*
	 * Other Nodes
	 */
	failed := 0
	var serverlb *k3d.Node
	for _, node := range cluster.Nodes {

		// skip the LB, because we want to start it last
		if node.Role == k3d.LoadBalancerRole {
			serverlb = node
			continue
		}

		// check if node is running already to avoid waiting forever when checking for the node log message
		if !node.State.Running {

			// start node
			if err := NodeStart(ctx, runtime, node, k3d.NodeStartOpts{
				NodeHooks: startClusterOpts.NodeHooks,
			}); err != nil {
				log.Warningf("Failed to start node '%s': Try to start it manually", node.Name)
				failed++
				continue
			}

			// wait for this server node to be ready (by checking the logs for a specific log message)
			if node.Role == k3d.ServerRole && startClusterOpts.WaitForServer {
				log.Debugf("Waiting for server node '%s' to get ready", node.Name)
				if err := NodeWaitForLogMessage(ctx, runtime, node, k3d.ReadyLogMessageByRole[k3d.ServerRole], start); err != nil {
					return fmt.Errorf("Server node '%s' failed to get ready: %+v", node.Name, err)
				}
			}

		} else {
			log.Infof("Node '%s' already running", node.Name)
		}
	}

	// start serverlb
	if serverlb != nil {
		if !serverlb.State.Running {
			log.Debugln("Starting serverlb...")
			if err := runtime.StartNode(ctx, serverlb); err != nil { // FIXME: we could run into a nullpointer exception here
				log.Warningf("Failed to start serverlb '%s': Try to start it manually", serverlb.Name)
				failed++
			}
			// TODO: avoid `level=fatal msg="starting kubernetes: preparing server: post join: a configuration change is already in progress (5)"`
			// ... by scanning for this line in logs and restarting the container in case it appears
			log.Debugf("Starting to wait for loadbalancer node '%s'", serverlb.Name)
			if err := NodeWaitForLogMessage(ctx, runtime, serverlb, k3d.ReadyLogMessageByRole[k3d.LoadBalancerRole], start); err != nil {
				return fmt.Errorf("Loadbalancer '%s' failed to get ready: %+v", serverlb.Name, err)
			}
		} else {
			log.Infof("Serverlb '%s' already running", serverlb.Name)
		}
	}

	if failed > 0 {
		return fmt.Errorf("Failed to start %d nodes: Try to start them manually", failed)
	}
	return nil
}

// ClusterStop stops a whole cluster (i.e. all nodes of the cluster)
func ClusterStop(ctx context.Context, runtime k3drt.Runtime, cluster *k3d.Cluster) error {
	log.Infof("Stopping cluster '%s'", cluster.Name)

	failed := 0
	for _, node := range cluster.Nodes {
		if err := runtime.StopNode(ctx, node); err != nil {
			log.Warningf("Failed to stop node '%s': Try to stop it manually", node.Name)
			failed++
			continue
		}
	}

	if failed > 0 {
		return fmt.Errorf("Failed to stop %d nodes: Try to stop them manually", failed)
	}
	return nil
}

// SortClusters : in place sort cluster list by cluster name alphabetical order
func SortClusters(clusters []*k3d.Cluster) []*k3d.Cluster {
	sort.Slice(clusters, func(i, j int) bool {
		return clusters[i].Name < clusters[j].Name
	})
	return clusters
}

// prepInjectHostIP adds /etc/hosts and CoreDNS entry for host.k3d.internal, referring to the host system
func prepInjectHostIP(ctx context.Context, runtime k3drt.Runtime, cluster *k3d.Cluster) {
	log.Infoln("(Optional) Trying to get IP of the docker host and inject it into the cluster as 'host.k3d.internal' for easy access")
	hostIP, err := GetHostIP(ctx, runtime, cluster)
	if err != nil {
		log.Warnf("Failed to get HostIP: %+v", err)
	}
	if hostIP != nil {
		hostRecordSuccessMessage := ""
		etcHostsFailureCount := 0
		hostsEntry := fmt.Sprintf("%s %s", hostIP, k3d.DefaultK3dInternalHostRecord)
		log.Debugf("Adding extra host entry '%s'...", hostsEntry)
		for _, node := range cluster.Nodes {
			if err := runtime.ExecInNode(ctx, node, []string{"sh", "-c", fmt.Sprintf("echo '%s' >> /etc/hosts", hostsEntry)}); err != nil {
				log.Warnf("Failed to add extra entry '%s' to /etc/hosts in node '%s'", hostsEntry, node.Name)
				etcHostsFailureCount++
			}
		}
		if etcHostsFailureCount < len(cluster.Nodes) {
			hostRecordSuccessMessage += fmt.Sprintf("Successfully added host record to /etc/hosts in %d/%d nodes", (len(cluster.Nodes) - etcHostsFailureCount), len(cluster.Nodes))
		}

		patchCmd := `test=$(kubectl get cm coredns -n kube-system --template='{{.data.NodeHosts}}' | sed -n -E -e '/[0-9\.]{4,12}\s+host\.k3d\.internal$/!p' -e '$a` + hostsEntry + `' | tr '\n' '^' | busybox xargs -0 printf '{"data": {"NodeHosts":"%s"}}'| sed -E 's%\^%\\n%g') && kubectl patch cm coredns -n kube-system -p="$test"`
		if err = runtime.ExecInNode(ctx, cluster.Nodes[0], []string{"sh", "-c", patchCmd}); err != nil {
			log.Warnf("Failed to patch CoreDNS ConfigMap to include entry '%s': %+v", hostsEntry, err)
		} else {
			hostRecordSuccessMessage += " and to the CoreDNS ConfigMap"
		}

		if hostRecordSuccessMessage != "" {
			log.Infoln(hostRecordSuccessMessage)
		}

	}
}

func prepCreateLocalRegistryHostingConfigMap(ctx context.Context, runtime k3drt.Runtime, cluster *k3d.Cluster) error {
	success := false
	for _, node := range cluster.Nodes {
		if node.Role == k3d.AgentRole || node.Role == k3d.ServerRole {
			err := runtime.ExecInNode(ctx, node, []string{"sh", "-c", "kubectl apply -f /tmp/reg.yaml"})
			if err == nil {
				success = true
				break
			} else {
				log.Debugf("Failed to create LocalRegistryHosting ConfigMap in node %s: %+v", node.Name, err)
			}
		}
	}
	if success == false {
		log.Warnf("Failed to create LocalRegistryHosting ConfigMap")
	}
	return nil
}
