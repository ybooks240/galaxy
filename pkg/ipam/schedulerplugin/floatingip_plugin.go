package schedulerplugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"git.code.oa.com/gaiastack/galaxy/pkg/api/k8s/schedulerapi"
	"git.code.oa.com/gaiastack/galaxy/pkg/ipam/floatingip"
	"git.code.oa.com/gaiastack/galaxy/pkg/utils/database"
	"github.com/golang/glog"
	"k8s.io/client-go/1.4/pkg/api"
	k8serrs "k8s.io/client-go/1.4/pkg/api/errors"
	"k8s.io/client-go/1.4/pkg/api/v1"
	gaiav1 "k8s.io/client-go/1.4/pkg/apis/gaia/v1alpha1"
	"k8s.io/client-go/1.4/pkg/labels"
	"k8s.io/client-go/1.4/pkg/runtime"
	"k8s.io/client-go/1.4/pkg/util/sets"
	"k8s.io/client-go/1.4/pkg/util/wait"
)

var (
	ANNOTATION_FLOATINGIP = "floatingip"
)

type Conf struct {
	FloatingIPs        []*floatingip.FloatingIP `json:"floatingips,omitempty"`
	DBConfig           *database.DBConfig       `json:"database"`
	ResyncInterval     uint                     `json:"resyncInterval"`
	ConfigMapName      string                   `json:"configMapName"`
	ConfigMapNamespace string                   `json:"configMapNamespace"`
}

// FloatingIPPlugin Allocates Floating IP for deployments
type FloatingIPPlugin struct {
	objectSelector, nodeSelector labels.Selector
	// whether or not the deployment wants its allocated floatingips invariant accross pod reassigning
	fipInvariantSeletor labels.Selector
	ipam                floatingip.IPAM
	// node name to subnet cache
	nodeSubnet     map[string]*net.IPNet
	nodeSubnetLock sync.Mutex
	sync.Mutex
	*PluginFactoryArgs
	lastFIPConf string
	conf        *Conf
	unreleased  chan *v1.Pod
}

func NewFloatingIPPlugin(conf Conf, args *PluginFactoryArgs) (*FloatingIPPlugin, error) {
	if conf.ResyncInterval < 1 {
		conf.ResyncInterval = 1
	}
	if conf.ConfigMapName == "" {
		conf.ConfigMapName = "floatingip-config"
	}
	if conf.ConfigMapNamespace == "" {
		conf.ConfigMapNamespace = "kube-system"
	}
	glog.Infof("floating ip config: %v", conf)
	db := database.NewDBRecorder(conf.DBConfig)
	if err := db.Run(); err != nil {
		return nil, err
	}
	ipam := floatingip.NewIPAM(db)
	plugin := &FloatingIPPlugin{
		ipam:              ipam,
		nodeSubnet:        make(map[string]*net.IPNet),
		PluginFactoryArgs: args,
		conf:              &conf,
		unreleased:        make(chan *v1.Pod, 10),
	}
	plugin.initSelector()
	if len(conf.FloatingIPs) > 0 {
		if err := ipam.ConfigurePool(conf.FloatingIPs); err != nil {
			return nil, err
		}
	} else {
		glog.Infof("empty floatingips from config, fetching from configmap")
		if err := wait.PollInfinite(time.Millisecond*100, func() (done bool, err error) {
			updated, err := plugin.updateConfigMap()
			if err != nil {
				glog.Warning(err)
			}
			return updated, nil
		}); err != nil {
			return nil, fmt.Errorf("failed to get floatingip config from configmap: %v", err)
		}
		go wait.Forever(func() {
			if _, err := plugin.updateConfigMap(); err != nil {
				glog.Warning(err)
			}
		}, time.Minute)
	}
	go wait.Forever(func() {
		if err := plugin.resyncPod(); err != nil {
			glog.Warning(err)
		}
	}, time.Duration(conf.ResyncInterval)*time.Minute)
	go plugin.loop()
	return plugin, nil
}

func (p *FloatingIPPlugin) initSelector() error {
	objectSelectorMap := make(map[string]string)
	objectSelectorMap["network"] = "FLOATINGIP"
	nodeSelectorMap := make(map[string]string)
	nodeSelectorMap["network"] = "floatingip"

	fipInvariantLabelMap := make(map[string]string)
	fipInvariantLabelMap["floatingip"] = "invariant"

	labels.SelectorFromSet(labels.Set(objectSelectorMap))
	p.objectSelector = labels.SelectorFromSet(labels.Set(objectSelectorMap))
	p.nodeSelector = labels.SelectorFromSet(labels.Set(nodeSelectorMap))
	p.fipInvariantSeletor = labels.SelectorFromSet(labels.Set(fipInvariantLabelMap))
	return nil
}

// updateConfigMap fetches the newest floatingips configmap and syncs in memory/db config,
// returns true if updated.
func (p *FloatingIPPlugin) updateConfigMap() (bool, error) {
	cm, err := p.Client.ConfigMaps(p.conf.ConfigMapNamespace).Get(p.conf.ConfigMapName)
	if err != nil {
		return false, fmt.Errorf("failed to get floatingip configmap %s_%s: %v", p.conf.ConfigMapName, p.conf.ConfigMapNamespace, err)

	}
	val, ok := cm.Data["floatingips"]
	if !ok {
		return false, fmt.Errorf("configmap %s_%s doesn't have a key floatingips", p.conf.ConfigMapName, p.conf.ConfigMapNamespace)
	}
	if val == p.lastFIPConf {
		glog.V(4).Infof("floatingip configmap unchanged")
		return false, nil
	}
	glog.Infof("updating floatingip config %s", val)
	var conf []*floatingip.FloatingIP
	if err := json.Unmarshal([]byte(val), &conf); err != nil {
		return false, fmt.Errorf("failed to unmarshal configmap %s_%s val %s to floatingip config", p.conf.ConfigMapName, p.conf.ConfigMapNamespace, val)
	}
	p.lastFIPConf = val
	if err := p.ipam.ConfigurePool(conf); err != nil {
		glog.Warningf("failed to configure pool: %v", err)
	}
	return true, nil
}

// Filter marks nodes which haven't been labeled as supporting floating IP or have no available ips as FailedNodes
// If the given pod doesn't want floating IP, none failedNodes returns
func (p *FloatingIPPlugin) Filter(pod *v1.Pod, nodes []v1.Node) ([]v1.Node, schedulerapi.FailedNodesMap, error) {
	failedNodesMap := schedulerapi.FailedNodesMap{}
	if !p.wantedObject(&pod.ObjectMeta) {
		return nodes, failedNodesMap, nil
	}
	filteredNodes := []v1.Node{}
	var (
		subnets []string
		err     error
	)
	key := keyInDB(pod)
	if subnets, err = p.ipam.QueryRoutableSubnetByKey(key); err != nil {
		return filteredNodes, failedNodesMap, fmt.Errorf("failed to query by key %s: %v", key, err)
	}
	if len(subnets) != 0 {
		glog.V(3).Infof("%s already have an allocated floating ip in subnets %v, it may have been deleted or evicted", key, subnets)
	} else {
		if subnets, err = p.ipam.QueryRoutableSubnetByKey(""); err != nil {
			return filteredNodes, failedNodesMap, fmt.Errorf("failed to query allocatable subnet: %v", err)
		}
	}
	subsetSet := sets.NewString(subnets...)
	for i := range nodes {
		nodeName := nodes[i].Name
		if !p.nodeSelector.Matches(labels.Set(nodes[i].GetLabels())) {
			failedNodesMap[nodeName] = "FloatingIPPlugin:UnlabelNode"
			continue
		}
		subnet, err := p.getNodeSubnet(&nodes[i])
		if err != nil {
			failedNodesMap[nodes[i].Name] = err.Error()
			continue
		}
		if subsetSet.Has(subnet.String()) {
			filteredNodes = append(filteredNodes, nodes[i])
		} else {
			failedNodesMap[nodeName] = "FloatingIPPlugin:NoFIPLeft"
		}
	}
	if bool(glog.V(4)) {
		nodeNames := make([]string, len(filteredNodes))
		for i := range filteredNodes {
			nodeNames[i] = filteredNodes[i].Name
		}
		glog.V(4).Infof("filtered nodes %v failed nodes %v", nodeNames, failedNodesMap)
	}
	return filteredNodes, failedNodesMap, nil
}

func (p *FloatingIPPlugin) Prioritize(pod *v1.Pod, nodes []v1.Node) (*schedulerapi.HostPriorityList, error) {
	list := &schedulerapi.HostPriorityList{}
	if !p.wantedObject(&pod.ObjectMeta) {
		return list, nil
	}
	//TODO
	return list, nil
}

// allocateIP Allocates a floating IP to the pod based on the winner node name
func (p *FloatingIPPlugin) allocateIP(key, nodeName string) (map[string]string, error) {
	var how string
	ipInfo, err := p.ipam.QueryFirst(key)
	if err != nil {
		return nil, fmt.Errorf("failed to query floating ip by key %s: %v", key, err)
	}
	if ipInfo != nil {
		how = "reused"
		glog.V(3).Infof("pod %s may have been deleted or evicted, it already have an allocated floating ip %s", key, ipInfo.IP.String())
	} else {
		subnet, err := p.queryNodeSubnet(nodeName)
		_, err = p.ipam.AllocateInSubnet(key, subnet)
		if err != nil {
			// return this error directly, invokers depend on the error type if it is ErrNoEnoughIP
			return nil, err
		}
		how = "allocated"
		ipInfo, err = p.ipam.QueryFirst(key)
		if err != nil {
			return nil, fmt.Errorf("failed to query floating ip by key %s: %v", key, err)
		}
		if ipInfo == nil {
			return nil, fmt.Errorf("nil floating ip for key %s: %v", key, err)
		}
	}
	data, err := json.Marshal(ipInfo)
	if err != nil {
		return nil, err
	}
	glog.Infof("%s floating ip %s for %s", how, ipInfo.IP.String(), key)
	bind := make(map[string]string)
	bind[ANNOTATION_FLOATINGIP] = string(data)
	return bind, nil
}

func (p *FloatingIPPlugin) releasePodIP(key string) error {
	ipInfo, err := p.ipam.QueryFirst(key)
	if err != nil {
		return fmt.Errorf("failed to query floating ip of %s: %v", key, err)
	}
	if ipInfo == nil {
		return nil
	}
	if err := p.ipam.Release([]string{key}); err != nil {
		return fmt.Errorf("failed to release floating ip of %s: %v", key, err)
	}
	glog.Infof("released floating ip %s from %s", ipInfo.IP.String(), key)
	return nil
}

func (p *FloatingIPPlugin) AddPod(pod *v1.Pod) error {
	return nil
}

func (p *FloatingIPPlugin) Bind(args *schedulerapi.ExtenderBindingArgs) error {
	key := fmtKey(args.PodName, args.PodNamespace)
	bind, err := p.allocateIP(key, args.Node)
	if err != nil {
		return err
	}
	if bind == nil {
		return nil
	}
	ret := &runtime.Unstructured{}
	ret.SetAnnotations(bind)
	patchData, err := json.Marshal(ret)
	if err != nil {
		glog.Error(err)
	}
	if err := wait.PollImmediate(time.Millisecond*300, 20*time.Second, func() (bool, error) {
		_, err := p.Client.Pods(args.PodNamespace).Patch(args.PodName, api.MergePatchType, patchData)
		if err != nil {
			glog.Warningf("failed to update pod %s: %v", key, err)
			return false, nil
		}
		glog.V(3).Infof("updated %v for pod %s", bind["floatingip"], key)
		return true, nil
	}); err != nil {
		// If fails to update, depending on resync to update
		return fmt.Errorf("failed to update pod %s: %v", key, err)
	}
	return nil
}

func (p *FloatingIPPlugin) UpdatePod(oldPod, newPod *v1.Pod) error {
	if !p.wantedObject(&newPod.ObjectMeta) {
		return nil
	}
	if evicted(newPod) {
		// Deployments will leave evicted pods, while TApps don't
		// If it's a evicted one, release its ip
		p.unreleased <- newPod
	}
	return nil
}

func (p *FloatingIPPlugin) RemovePod(pod *v1.Pod) error {
	if !p.wantedObject(&pod.ObjectMeta) {
		return nil
	}
	p.unreleased <- pod
	return nil
}

func (p *FloatingIPPlugin) unbind(pod *v1.Pod) error {
	key := keyInDB(pod)
	if !p.fipInvariantSeletor.Matches(labels.Set(pod.GetLabels())) {
		return p.releasePodIP(key)
	} else {
		tapps, err := p.TAppLister.GetPodTApps(pod)
		if err != nil {
			return p.releasePodIP(key)
		}
		tapp := tapps[0]
		for i, status := range tapp.Spec.Statuses {
			if !tappInstanceKilled(status) || i != pod.Labels[gaiav1.TAppInstanceKey] {
				continue
			}
			// build the key namespace_tappname-id
			return p.releasePodIP(key)
		}
	}
	if pod.Annotations != nil {
		glog.V(3).Infof("reserved %s for pod %s", pod.Annotations[ANNOTATION_FLOATINGIP], key)
	}
	return nil
}

func (p *FloatingIPPlugin) releaseAppIPs(keyPrefix string) error {
	ipMap, err := p.ipam.QueryByPrefix(keyPrefix)
	if err != nil {
		return fmt.Errorf("failed to query allocated floating ips for app %s: %v", keyPrefix, err)
	}
	if err := p.ipam.ReleaseByPrefix(keyPrefix); err != nil {
		return fmt.Errorf("failed to release floating ip for app %s: %v", keyPrefix, err)
	} else {
		ips := []string{}
		for ip := range ipMap {
			ips = append(ips, ip)
		}
		glog.Infof("released all floating ip %v for %s", ips, keyPrefix)
	}
	return nil
}

func (p *FloatingIPPlugin) wantedObject(o *v1.ObjectMeta) bool {
	labelMap := o.GetLabels()
	if labelMap == nil {
		return false
	}
	if !p.objectSelector.Matches(labels.Set(labelMap)) {
		return false
	}
	return true
}

func getNodeIP(node *v1.Node) net.IP {
	for i := range node.Status.Addresses {
		if node.Status.Addresses[i].Type == v1.NodeInternalIP {
			return net.ParseIP(node.Status.Addresses[i].Address)
		}
	}
	return nil
}

func tappInstanceKilled(status gaiav1.InstanceStatus) bool {
	// TODO v1 INSTANCE_KILLED = "killed" but in types INSTANCE_KILLED = "Killed"
	return strings.ToLower(string(status)) == strings.ToLower(string(gaiav1.INSTANCE_KILLED))
}

func (p *FloatingIPPlugin) loop() {
	for {
		select {
		case pod := <-p.unreleased:
			go func() {
				if err := p.unbind(pod); err != nil {
					glog.Warning(err)
					// backoff time if required
					time.Sleep(300 * time.Millisecond)
					p.unreleased <- pod
				}
			}()
		}
	}
}

func evicted(pod *v1.Pod) bool {
	return pod.Status.Phase == v1.PodFailed && pod.Status.Reason == "Evicted"
}

func (p *FloatingIPPlugin) getNodeSubnet(node *v1.Node) (*net.IPNet, error) {
	p.nodeSubnetLock.Lock()
	defer p.nodeSubnetLock.Unlock()
	if subnet, ok := p.nodeSubnet[node.Name]; !ok {
		nodeIP := getNodeIP(node)
		if nodeIP == nil {
			return nil, errors.New("FloatingIPPlugin:UnknowNode")
		}
		if ipNet := p.ipam.RoutableSubnet(nodeIP); ipNet != nil {
			return ipNet, nil
		} else {
			return nil, errors.New("FloatingIPPlugin:NoFIPConfigNode")
		}
	} else {
		return subnet, nil
	}
}

func (p *FloatingIPPlugin) queryNodeSubnet(nodeName string) (*net.IPNet, error) {
	var (
		node *v1.Node
	)
	p.nodeSubnetLock.Lock()
	defer p.nodeSubnetLock.Unlock()
	if subnet, ok := p.nodeSubnet[nodeName]; !ok {
		if err := wait.Poll(time.Millisecond*100, time.Minute, func() (done bool, err error) {
			node, err = p.Client.Core().Nodes().Get(nodeName)
			if !k8serrs.IsServerTimeout(err) {
				return true, err
			}
			return false, nil
		}); err != nil {
			return nil, err
		}
		nodeIP := getNodeIP(node)
		if nodeIP == nil {
			return nil, errors.New("FloatingIPPlugin:UnknowNode")
		}
		if ipNet := p.ipam.RoutableSubnet(nodeIP); ipNet != nil {
			return ipNet, nil
		} else {
			return nil, errors.New("FloatingIPPlugin:NoFIPConfigNode")
		}
	} else {
		return subnet, nil
	}
}
