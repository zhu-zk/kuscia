// Copyright 2023 Ant Group Co., Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package provider

import (
	"fmt"
	"os"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"

	"github.com/secretflow/kuscia/pkg/agent/config"
	"github.com/secretflow/kuscia/pkg/agent/framework"
	"github.com/secretflow/kuscia/pkg/agent/kri"
	"github.com/secretflow/kuscia/pkg/agent/provider/node"
	"github.com/secretflow/kuscia/pkg/agent/provider/pod"
	"github.com/secretflow/kuscia/pkg/agent/resource"
	"github.com/secretflow/kuscia/pkg/utils/cgroup"
	"github.com/secretflow/kuscia/pkg/utils/kubeconfig"
	"github.com/secretflow/kuscia/pkg/utils/nlog"
)

type Factory interface {
	BuildNodeProvider() (kri.NodeProvider, error)
	BuildPodProvider(nodeName string, eventRecorder record.EventRecorder, resourceManager *resource.KubeResourceManager, podsController *framework.PodsController) (kri.PodProvider, error)
}

func NewFactory(agentConfig *config.AgentConfig, kubeClient kubernetes.Interface) (Factory, error) {
	switch agentConfig.Provider.Runtime {
	case config.ProcessRuntime:
		fallthrough
	case config.ContainerRuntime:
		return &containerRuntimeFactory{agentConfig: agentConfig}, nil
	case config.K8sRuntime:
		bkCfg := &agentConfig.Provider.K8s
		var (
			bkClient kubernetes.Interface
			err      error
		)
		if bkCfg.KubeconfigFile != "" {
			nlog.Infof("Create backend k8s client with kubeconfig file")
			bkClient, err = kubeconfig.CreateKubeClientFromKubeconfigWithOptions(bkCfg.KubeconfigFile, bkCfg.Endpoint, bkCfg.QPS, bkCfg.Burst, bkCfg.Timeout)
			if err != nil {
				return nil, err
			}
		} else {
			nlog.Infof("Create backend k8s client with in cluster config")

			inClusterConfig, err := rest.InClusterConfig()
			if err != nil {
				return nil, fmt.Errorf("failed to get in cluster config, detail-> %v", err)
			}

			bkClient, err = kubernetes.NewForConfig(inClusterConfig)
			if err != nil {
				return nil, fmt.Errorf("failed to create clientset for in cluster config, detail-> %v", err)
			}
		}

		return &k8sRuntimeFactory{agentConfig: agentConfig, kubeClient: kubeClient, bkClient: bkClient}, nil
	default:
		return nil, fmt.Errorf("unknown runtime: %s", agentConfig.Provider.Runtime)
	}
}

type containerRuntimeFactory struct {
	agentConfig *config.AgentConfig
}

func (f *containerRuntimeFactory) BuildNodeProvider() (kri.NodeProvider, error) {
	providerCfg := &f.agentConfig.Provider

	cm, err := node.NewCapacityManager(f.agentConfig.Provider.Runtime,
		&f.agentConfig.Capacity,
		&f.agentConfig.ReservedResources,
		f.agentConfig.RootDir,
		true)
	if err != nil {
		return nil, err
	}

	initCgroup(cm, f.agentConfig.Provider.Runtime)

	nodeDep := &node.GenericNodeDependence{
		BaseNodeDependence: node.BaseNodeDependence{
			Runtime:         providerCfg.Runtime,
			Namespace:       f.agentConfig.Namespace,
			Address:         f.agentConfig.NodeIP,
			CapacityManager: cm,
		},
		DiskPressurePath: f.agentConfig.DiskPressurePath,
	}

	nodeProvider := node.NewGenericNodeProvider(nodeDep)

	return nodeProvider, nil
}

func initCgroup(cm *node.CapacityManager, runtime string) {
	if cm == nil || (runtime != config.ProcessRuntime && runtime != config.ContainerRuntime) {
		return
	}

	if !cgroup.HasPermission() {
		return
	}

	set := func(cm *node.CapacityManager, runtime string) {
		for i := 0; ; i++ {
			err := setCgroup(cm, runtime)
			if err == nil {
				nlog.Infof("Finish initializing cgroup")
				return
			}

			if !os.IsNotExist(err) {
				nlog.Warnf("Init cgroup failed. details -> %v", err)
				return
			}

			time.Sleep(5 * time.Second)
		}
	}

	switch runtime {
	case config.ProcessRuntime:
		set(cm, runtime)
	case config.ContainerRuntime:
		// Asynchronously waiting for containerd to create cgroup
		go set(cm, runtime)
	default:
	}
}

func setCgroup(cm *node.CapacityManager, runtime string) error {
	switch runtime {
	case config.ProcessRuntime:
		m, err := newCgroupManager(cm, cgroup.KusciaAppsGroup)
		if err != nil {
			return err
		}
		return m.AddCgroup()
	case config.ContainerRuntime:
		m, err := newCgroupManager(cm, cgroup.K8sIOGroup)
		if err != nil {
			return err
		}
		return m.UpdateCgroup()
	default:
		nlog.Warnf("Unknown runtime %q, skip initializing cgroup", runtime)
	}
	return nil
}

func newCgroupManager(cm *node.CapacityManager, group string) (cgroup.Manager, error) {
	conf := &cgroup.Config{
		Group:       group,
		Pid:         0,
		CPUQuota:    cm.GetCgroupCPUQuota(),
		CPUPeriod:   cm.GetCgroupCPUPeriod(),
		MemoryLimit: cm.GetCgroupMemoryLimit(),
	}
	return cgroup.NewManager(conf)
}

func (f *containerRuntimeFactory) BuildPodProvider(nodeName string, eventRecorder record.EventRecorder, resourceManager *resource.KubeResourceManager, podsController *framework.PodsController) (kri.PodProvider, error) {
	podProviderDep := &pod.CRIProviderDependence{
		Namespace:        f.agentConfig.Namespace,
		NodeIP:           f.agentConfig.NodeIP,
		RootDirectory:    f.agentConfig.RootDir,
		StdoutDirectory:  f.agentConfig.StdoutPath,
		AllowPrivileged:  f.agentConfig.AllowPrivileged,
		NodeName:         nodeName,
		EventRecorder:    eventRecorder,
		ResourceManager:  resourceManager,
		PodStateProvider: podsController.GetPodStateProvider(),
		PodSyncHandler:   podsController,
		StatusManager:    podsController.GetStatusManager(),
		Runtime:          f.agentConfig.Provider.Runtime,
		CRIProviderCfg:   &f.agentConfig.Provider.CRI,
		RegistryCfg:      &f.agentConfig.Registry,
	}

	return pod.NewCRIProvider(podProviderDep)
}

type k8sRuntimeFactory struct {
	agentConfig *config.AgentConfig
	kubeClient  kubernetes.Interface
	bkClient    kubernetes.Interface
}

func (f *k8sRuntimeFactory) BuildNodeProvider() (kri.NodeProvider, error) {
	providerCfg := &f.agentConfig.Provider

	cm, err := node.NewCapacityManager(f.agentConfig.Provider.Runtime,
		&f.agentConfig.Capacity,
		&f.agentConfig.ReservedResources,
		f.agentConfig.RootDir,
		false)
	if err != nil {
		return nil, err
	}

	bkCfg := &f.agentConfig.Provider.K8s
	nodeDep := &node.K8sNodeDependence{
		BaseNodeDependence: node.BaseNodeDependence{
			Runtime:         providerCfg.Runtime,
			Namespace:       f.agentConfig.Namespace,
			Address:         f.agentConfig.NodeIP,
			CapacityManager: cm,
		},
		BkNamespace: bkCfg.Namespace,
		BkClient:    f.bkClient,
	}

	nodeProvider := node.NewK8sNodeProvider(nodeDep)
	return nodeProvider, nil
}

func (f *k8sRuntimeFactory) BuildPodProvider(nodeName string, eventRecorder record.EventRecorder, resourceManager *resource.KubeResourceManager, podsController *framework.PodsController) (kri.PodProvider, error) {
	bkCfg := &f.agentConfig.Provider.K8s

	podProviderDep := &pod.K8sProviderDependence{
		NodeName:        nodeName,
		Namespace:       f.agentConfig.Namespace,
		NodeIP:          f.agentConfig.NodeIP,
		StdoutDirectory: f.agentConfig.StdoutPath,
		KubeClient:      f.kubeClient,
		BkClient:        f.bkClient,
		PodSyncHandler:  podsController,
		ResourceManager: resourceManager,
		K8sProviderCfg:  bkCfg,
		Recorder:        eventRecorder,
	}

	return pod.NewK8sProvider(podProviderDep)
}
