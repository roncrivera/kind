/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cluster

import (
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/pkg/errors"

	"sigs.k8s.io/kind/pkg/cluster/config"
	"sigs.k8s.io/kind/pkg/cluster/kubeadm"
	"sigs.k8s.io/kind/pkg/cluster/nodes"
)

// KubeadmInitAction implements action for executing the kubadm init
// and a set of default post init operations like e.g. install the
// CNI network plugin.
type KubeadmInitAction struct{}

func init() {
	RegisterAction("init", NewKubeadmInitAction)
}

// NewKubeadmInitAction returns a new KubeadmInitAction
func NewKubeadmInitAction() Action {
	return &KubeadmInitAction{}
}

// Tasks returns the list of action tasks
func (b *KubeadmInitAction) Tasks() []Task {
	return []Task{
		{
			// Run kubeadm init on the BootstrapControlPlaneNode
			Description: "Starting Kubernetes (this may take a minute) ☸",
			TargetNodes: SelectBootstrapControlPlaneNode,
			Run:         runKubeadmInit,
		},
	}
}

// runKubeadmConfig executes kubadm init and a set of default
// post init operations.
func runKubeadmInit(ec *execContext, configNode *config.NodeReplica) error {
	// get the target node for this task
	node, ok := ec.NodeFor(configNode)
	if !ok {
		return fmt.Errorf("unable to get the handle for operating on node: %s", configNode.Name)
	}

	// run kubeadm
	if err := node.Command(
		// init because this is the control plane node
		"kubeadm", "init",
		// preflight errors are expected, in particular for swap being enabled
		// TODO(bentheelder): limit the set of acceptable errors
		"--ignore-preflight-errors=all",
		// specify our generated config file
		"--config=/kind/kubeadm.conf",
	).Run(); err != nil {
		return errors.Wrap(err, "failed to init node with kubeadm")
	}

	// copies the kubeconfig files locally in order to make the cluster
	// usable with kubectl.
	// the kubeconfig file created by kubeadm internally to the node
	// must be modified in order to use the random host port reserved
	// for the API server and exposed by the node

	// retrives the random host where the API server is exposed
	// TODO(fabrizio pandini): when external load-balancer will be
	//      implemented this should be modified accordingly
	hostPort, err := node.Ports(kubeadm.APIServerPort)
	if err != nil {
		return errors.Wrap(err, "failed to get kubeconfig from node")
	}

	kubeConfigPath := ec.KubeConfigPath()
	if err := node.WriteKubeConfig(kubeConfigPath, hostPort); err != nil {
		return errors.Wrap(err, "failed to get kubeconfig from node")
	}

	// install the CNI network plugin
	// TODO(bentheelder): support other overlay networks
	if err := node.Command(
		"/bin/sh", "-c",
		`kubectl apply --kubeconfig=/etc/kubernetes/admin.conf -f "https://cloud.weave.works/k8s/net?k8s-version=$(kubectl version --kubeconfig=/etc/kubernetes/admin.conf | base64 | tr -d '\n')"`,
	).Run(); err != nil {
		return errors.Wrap(err, "failed to apply overlay network")
	}

	// if we are only provisioning one node, remove the master taint
	// https://kubernetes.io/docs/setup/independent/create-cluster-kubeadm/#master-isolation
	if len(ec.config.AllReplicas()) == 1 {
		if err := node.Command(
			"kubectl", "--kubeconfig=/etc/kubernetes/admin.conf",
			"taint", "nodes", "--all", "node-role.kubernetes.io/master-",
		).Run(); err != nil {
			return errors.Wrap(err, "failed to remove master taint")
		}
	}

	// add the default storage class
	if err := addDefaultStorageClass(node); err != nil {
		return errors.Wrap(err, "failed to add default storage class")
	}

	// Wait for the control plane node to reach Ready status.
	isReady := nodes.WaitForReady(node, time.Now().Add(ec.waitForReady))
	if ec.waitForReady > 0 {
		if !isReady {
			log.Warn("timed out waiting for control plane to be ready")
		}
	}

	return nil
}

func addDefaultStorageClass(controlPlane *nodes.Node) error {
	in := strings.NewReader(defaultStorageClassManifest)
	cmd := controlPlane.Command(
		"kubectl",
		"--kubeconfig=/etc/kubernetes/admin.conf", "apply", "-f", "-",
	)
	cmd.SetStdin(in)
	return cmd.Run()
}
