package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const podGone = "Gone"

func askForConfirmation() {
	fmt.Printf("Please confirm: ")
	defer fmt.Println("")

	buf := make([]byte, 1)

	for {
		if _, err := os.Stdin.Read(buf); err != nil {
			fmt.Println("Error reading:", err)
		}

		if buf[0] == 'y' {
			break
		}
	}
}

func (app *app) confirm() {
	if app.config.confirm {
		askForConfirmation()
	}
}

func (app *app) willPvcWillBlockNextPodCreation(pod *v1.Pod) (bool, error) {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}

		claim, err := app.client.CoreV1().PersistentVolumeClaims(pod.Namespace).Get(
			context.TODO(),
			v.PersistentVolumeClaim.ClaimName,
			metav1.GetOptions{},
		)

		if err != nil {
			app.log.Errorw("Couldn't fetch PV", "pvName", v.PersistentVolumeClaim.ClaimName, "err", err)

			return false, fmt.Errorf("couldn't fetch PV %s: %w", v.PersistentVolumeClaim.ClaimName, err)
		}

		for _, mode := range claim.Spec.AccessModes {
			if mode == v1.ReadWriteOnce {
				return true, nil
			}
		}
	}

	return false, nil
}

func (app *app) processKindReplicaSet(log *zap.SugaredLogger, pod *v1.Pod, or *metav1.OwnerReference) error {
	log.Info("Changing ReplicaSet")

	app.confirm()
	_, err := app.client.AppsV1().ReplicaSets(pod.Namespace).Patch(
		context.TODO(),
		or.Name,
		types.MergePatchType,
		[]byte(fmt.Sprintf(`{"spec":{"template":{"metadata":{"labels":{"cleanup-cordoned":"%s"}}}}}`, app.config.patchID)),
		metav1.PatchOptions{},
	)

	if err != nil {
		return fmt.Errorf("couldn't patch ReplicaSet %s: %w", or.Name, err)
	}

	return nil
}

func (app *app) processKindStatefulSet(log *zap.SugaredLogger, pod *v1.Pod, or *metav1.OwnerReference) error {
	log.Info("Changing StatefulSet")

	app.confirm()
	_, err := app.client.AppsV1().StatefulSets(pod.Namespace).Patch(
		context.TODO(),
		or.Name,
		types.MergePatchType,
		[]byte(fmt.Sprintf(`{"spec":{"template":{"metadata":{"labels":{"cleanup-cordoned":"%s"}}}}}`, app.config.patchID)),
		metav1.PatchOptions{},
	)

	if err != nil {
		return fmt.Errorf("couldn't patch StatefulSet %s: %w", or.Name, err)
	}

	return nil
}

func (app *app) processPodWithPV(log *zap.SugaredLogger, pod *v1.Pod) (bool, error) {
	if shouldDeletePod, err := app.willPvcWillBlockNextPodCreation(pod); err == nil && shouldDeletePod {
		// We could also check that there's an other pod in a "ContainerCreating" status in the same replicaset so that
		// we cause minimal disruption.
		log.Warnw("Deleting pod !!!")

		app.confirm()

		if err := app.client.CoreV1().Pods(pod.Namespace).Delete(
			context.TODO(), pod.Name, metav1.DeleteOptions{}); err != nil {
			log.Errorw("Couldn't delete pod", "err", err)

			return true, fmt.Errorf("couldn't delete pod %s: %w", pod.Name, err)
		}

		return true, nil
	}

	return true, nil
}

// ProcessPod tries to change the owner of the pod
func (app *app) processPod(pod *v1.Pod) (bool, error) {
	log := app.log.With("podName", pod.Name)

	if pod.Status.Phase != v1.PodRunning && pod.Status.Phase != v1.PodPending {
		log.Infow("Ignoring because of status", "podStatus", pod.Status.Phase)

		return false, nil
	}

	for _, o := range pod.ObjectMeta.OwnerReferences {
		ownerRef := o

		var err error

		switch ownerRef.Kind {
		case "ReplicaSet":
			err = app.processKindReplicaSet(log, pod, &ownerRef)
		case "StatefulSet":
			err = app.processKindStatefulSet(log, pod, &ownerRef)
		case "DaemonSet":
			{
				// We could evict the pods here but it will probably lead to undesired behavior, if we do it we should
				// do it *after* the pods not belonging to a daemonset have been dropped.
				log.Info("Ignoring because it's a daemonset")

				return false, nil
			}
		default:
			{
				log.Warn("Unknown owner type", "kind", ownerRef.Kind)

				// If we don't know the owner type, we can't safely do anything, so we'll just
				// wait like this forever. This is either a bug in the code or something a human
				// must handle.
				return true, nil
			}
		}

		if err != nil {
			return true, err
		}
	}

	// https://kubernetes.io/docs/concepts/storage/persistent-volumes/#access-modes
	if app.config.deletePodsWithPV {
		return app.processPodWithPV(log, pod)
	}

	return true, nil
}

const timeWaitingForPods = 20 * time.Second

func (app *app) waitForPods(podsToWaitFor []v1.Pod, nodeName string) {
	log := app.log.With("nodeName", nodeName)
	log.Info("Waiting for pods to disappear on node")

	for {
		nbPodsStillAlive := 0

		for k := range podsToWaitFor {
			pod := &podsToWaitFor[k]
			if pod.Status.Phase == podGone {
				continue
			}

			updatedPod, err := app.client.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
			if err != nil {
				if k8serrors.IsNotFound(err) {
					pod.Status.Phase = podGone
				} else {
					log.Warnw("Error getting pod", "err", err)

					break
				}
			} else if updatedPod.Status.Phase == v1.PodRunning && updatedPod.Spec.NodeName != pod.Spec.NodeName {
				log.Info(
					"Same pod but different host",
					"podName", pod.Name,
					"nodeName", updatedPod.Spec.NodeName,
					"previousNodeName", pod.Spec.NodeName,
				)
				pod.Status.Phase = podGone
			}

			log.Infow("Pod status", "podName", pod.Name, "podStatus", pod.Status.Phase)

			if updatedPod.Status.Phase == v1.PodRunning || updatedPod.Status.Phase == v1.PodPending {
				nbPodsStillAlive++
			}
		}

		if nbPodsStillAlive == 0 {
			log.Info("All pods are gone now")

			break
		} else {
			log.Infow("Waiting for pods", "nbPods", nbPodsStillAlive)
			time.Sleep(timeWaitingForPods)
		}
	}
}

func (app *app) movePodsFromNode(node *v1.Node) (int, error) {
	log := app.log.With("nodeName", node.Name)
	log.Infow("Moving pods from node")

	podsList, err := app.client.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + node.Name,
	})

	if err != nil {
		return -1, fmt.Errorf("couldn't list pods: %w", err)
	}

	podsToWaitFor := make([]v1.Pod, 0)

	log.Info("Considering all the pods")

	for _, p := range podsList.Items {
		pod := p // To avoid the loop variable being reused elsewhere (implicit memory aliasing)
		if result, err := app.processPod(&pod); err != nil {
			log.Warnw("Couldn't process pod", "err", err)
		} else if result {
			podsToWaitFor = append(podsToWaitFor, pod)
		}
	}

	app.waitForPods(podsToWaitFor, node.Name)

	return len(podsToWaitFor), nil
}

func (app *app) cordonNode(node *v1.Node) error {
	log := app.log.With("nodeName", node.Name)

	log.Infow("Cordoning node", "nodeName", node.Name)
	app.confirm()

	if _, err := app.client.CoreV1().Nodes().Patch(
		context.TODO(),
		node.Name,
		types.MergePatchType,
		[]byte(`{"spec":{"unschedulable":true}}`),
		metav1.PatchOptions{},
	); err != nil {
		return fmt.Errorf("couldn't patch node %s: %w", node.Name, err)
	}

	return nil
}

const k8sLabelsSplit = 2

func (app *app) findANodeToCordon() (bool, error) {
	app.log.Infow(
		"Finding a node to cordon",
		"kubeletVersion", app.config.nodeCordonKubeletVersion,
		"label", app.config.nodeCordonLabelsMatch,
	)

	nodesList, err := app.client.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})

	if err != nil {
		return false, fmt.Errorf("couldn't list nodes: %w", err)
	}

nodesIteration:
	for _, n := range nodesList.Items {
		node := n // To avoid the loop variable being reused elsewhere (implicit memory aliasing)
		log := app.log.With("nodeName", node.Name)
		if node.Spec.Unschedulable {
			continue
		}

		if app.config.nodeCordonKubeletVersion != "" &&
			app.config.nodeCordonKubeletVersion != node.Status.NodeInfo.KubeletVersion {
			log.Debugw(
				"Node has a non matching kubeletVersion",
				"nodeName", node.Name,
				"kubeletVersion", node.Status.NodeInfo.KubeletVersion,
			)

			continue
		}

		{ // Matching on all labels
			var labelName, labelValue string
			for _, labelRaw := range app.config.nodeCordonLabelsMatch {
				spl := strings.SplitN(labelRaw, "=", k8sLabelsSplit)
				labelName = spl[0]
				labelValue = spl[1]

				if labelValue != "" && node.Labels[labelName] != labelValue {
					log.Debugw("Node didn't match our label", "labelMatch", labelName+"="+labelValue)

					continue nodesIteration
				}
			}
		}

		if err := app.cordonNode(&node); err != nil {
			app.log.Warnw("Couldn't cordon node", "nodeName", node.Name, "err", err)

			return false, err
		}

		return true, nil
	}

	return false, nil
}

type app struct {
	config config
	client *kubernetes.Clientset
	log    *zap.SugaredLogger
}

func (app *app) initLogging() error {
	config := zap.NewDevelopmentConfig()
	config.Development = false
	config.DisableCaller = true
	logger, err := config.Build()

	if err != nil {
		return fmt.Errorf("couldn't init logging: %w", err)
	}

	app.log = logger.Sugar()

	return nil
}

func (app *app) initClient() error {
	kubeConfig := filepath.Join(os.Getenv("HOME"), ".kube", "config")
	config, err := clientcmd.BuildConfigFromFlags("", kubeConfig)

	if err != nil {
		return fmt.Errorf("couldn't build config from flags: %w", err)
	}

	app.client, err = kubernetes.NewForConfig(config)

	if err != nil {
		return fmt.Errorf("couldn't create client: %w", err)
	}

	return nil
}

func (app *app) movePodsAwayFromCordonnedNodes() (int, error) {
	nodesList, err := app.client.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{
		FieldSelector: "spec.unschedulable=true",
	})

	if err != nil {
		return 0, fmt.Errorf("couldn't list nodes: %w", err)
	}

	nbMovedPods := 0

	for _, n := range nodesList.Items {
		node := n
		if nbPods, err := app.movePodsFromNode(&node); err == nil {
			nbMovedPods += nbPods
		}

		log := app.log.With("nodeName", node.Name)

		if app.config.deleteNode {
			log.Warnw("Deleting node")

			if err := app.client.CoreV1().Nodes().Delete(context.TODO(), node.Name, metav1.DeleteOptions{}); err != nil {
				log.Errorw("Couldn't delete node:", "err", err)
			}
		}
	}

	return nbMovedPods, nil
}

func (app *app) cordonNodesAndMovePods() error {
	for nbPass := 0; ; nbPass++ {
		app.log.Infow("Moving pods", "passNb", nbPass)

		if nbPods, err := app.movePodsAwayFromCordonnedNodes(); err != nil {
			app.log.Warnw("Problem moving pods", "err", err)
		} else {
			app.log.Infow("Moved some pods", "nbPods", nbPods)
		}

		if found, err := app.findANodeToCordon(); err != nil {
			app.log.Errorw("Problem finding a node", "err", err)

			// WARNING: Added this during linting, it might change behavior
			return fmt.Errorf("problem finding a node: %w", err)
		} else if !found {
			app.log.Info("No more pods to find")

			return nil
		}
	}
}

func main() {
	var app app

	var err error

	app.config.Parse()

	if err = app.initLogging(); err != nil {
		fmt.Println("Couldn't init logs:", err)
	}

	app.log.Info("K8s nodes cleaner")

	if err = app.initClient(); err != nil {
		app.log.Panicw("Couldn't init client", "err", err)
	}

	if app.config.wantsToDoNodeCordoning() {
		err = app.cordonNodesAndMovePods()
	} else {
		_, err = app.movePodsAwayFromCordonnedNodes()
	}

	if err != nil {
		app.log.Errorw("Problem moving pods", "err", err)
	}
}
