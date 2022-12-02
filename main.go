package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	v1 "k8s.io/api/core/v1"
)

const PodGone = "Gone"

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
			return false, err
		}
		for _, mode := range claim.Spec.AccessModes {
			if mode == v1.ReadWriteOnce {
				return true, nil
			}
		}
	}
	return false, nil
}

// ProcessPod tries to change the owner of the pod
func (app *app) processPod(pod *v1.Pod) (bool, error) {
	log := app.log.With("podName", pod.Name)

	if pod.Status.Phase != v1.PodRunning && pod.Status.Phase != v1.PodPending {
		log.Infow("Ignoring because of status", "podStatus", pod.Status.Phase)
		return false, nil
	}

	for _, or := range pod.ObjectMeta.OwnerReferences {
		if or.Kind == "ReplicaSet" {
			log.Info("Changing ReplicaSet")
			app.confirm()
			_, err := app.client.AppsV1().ReplicaSets(pod.Namespace).Patch(
				context.TODO(),
				or.Name,
				types.MergePatchType,
				[]byte(fmt.Sprintf(`{"spec":{"template":{"metadata":{"labels":{"cleanup-cordoned":"%s"}}}}}`, app.config.patchId)),
				metav1.PatchOptions{},
			)

			// https://kubernetes.io/docs/concepts/storage/persistent-volumes/#access-modes
			if app.config.deletePodsWithPv {
				if shouldDeletePod, err := app.willPvcWillBlockNextPodCreation(pod); err == nil && shouldDeletePod {
					// We could also check that there's an other pod in a "ContainerCreating" status in the same replicaset so that
					// we cause minimal disruption.
					log.Warnw("Deleting pod !!!")
					app.confirm()
					if err := app.client.CoreV1().Pods(pod.Namespace).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{}); err != nil {
						log.Errorw("Couldn't delete pod", "err", err)
					}
				}
			}
			return true, err
		} else if or.Kind == "StatefulSet" {
			log.Info("Changing StatefulSet")
			app.confirm()
			_, err := app.client.AppsV1().StatefulSets(pod.Namespace).Patch(
				context.TODO(),
				or.Name,
				types.MergePatchType,
				[]byte(fmt.Sprintf(`{"spec":{"template":{"metadata":{"labels":{"cleanup-cordoned":"%s"}}}}}`, app.config.patchId)),
				metav1.PatchOptions{},
			)
			return true, err
		} else if or.Kind == "DaemonSet" {
			// We could evict the pods here but it will probably lead to undesired behavior, if we do it we should
			// do it *after* the pods not belonging to a daemonset have been dropped.
			log.Info("Ignoring because it's a daemonset")
		} else {
			log.Warn("Unknown owner type", "kind", or.Kind)

			// We'll wait for the pods because if this happens, we're probably missing something and it's better to wait forever in this case.
			return true, nil
		}
	}

	return false, nil
}

func (app *app) waitForPods(podsToWaitFor []v1.Pod, nodeName string) {
	log := app.log.With("nodeName", nodeName)
	log.Info("Waiting for pods to disappear on node")
	for {
		nbPodsStillAlive := 0
		for k := range podsToWaitFor {
			pod := &podsToWaitFor[k]
			if pod.Status.Phase == PodGone {
				continue
			}
			updatedPod, err := app.client.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
			if err != nil {
				if k8serrors.IsNotFound(err) {
					pod.Status.Phase = PodGone
				} else {
					log.Warnw("Error getting pod", "err", err)
					break
				}
			} else if updatedPod.Status.Phase == v1.PodRunning && updatedPod.Spec.NodeName != pod.Spec.NodeName {
				log.Info("Same pod but different host", "podName", pod.Name, "nodeName", updatedPod.Spec.NodeName, "previousNodeName", pod.Spec.NodeName)
				pod.Status.Phase = PodGone
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
			time.Sleep(time.Second * 10)
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
		return -1, err
	}

	podsToWaitFor := make([]v1.Pod, 0)

	log.Info("Considering all the pods")
	for _, pod := range podsList.Items {
		if result, err := app.processPod(&pod); err != nil {
			log.Warnw("Couldn't list pod", "err", err)
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

	_, err := app.client.CoreV1().Nodes().Patch(
		context.TODO(),
		node.Name,
		types.MergePatchType,
		[]byte(`{"spec":{"unschedulable":true}}`),
		metav1.PatchOptions{},
	)

	return err
}

func (app *app) findANodeToCordon() (bool, error) {
	app.log.Infow(
		"Finding a node to cordon",
		"kubeletVersion", app.config.nodeCordonKubeletVersion,
		"label", app.config.nodeCordonLabelsMatch,
	)

	nodesList, err := app.client.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})

	if err != nil {
		return false, err
	}

nodesIteration:
	for _, node := range nodesList.Items {
		log := app.log.With("nodeName", node.Name)
		if node.Spec.Unschedulable {
			continue
		}

		if app.config.nodeCordonKubeletVersion != "" && app.config.nodeCordonKubeletVersion != node.Status.NodeInfo.KubeletVersion {
			log.Debugw("Node has a non matching kubeletVersion", "nodeName", node.Name, "kubeletVersion", node.Status.NodeInfo.KubeletVersion)
			continue
		}

		{ // Matching on all labels
			var labelName, labelValue string
			for _, labelRaw := range app.config.nodeCordonLabelsMatch {
				spl := strings.SplitN(labelRaw, "=", 2)
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
		} else {
			return true, nil
		}
	}

	return false, nil
}

type arrayFlags []string

func (i *arrayFlags) String() string {
	return fmt.Sprintf("%#v", *i)
}

func (i *arrayFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

type config struct {
	deleteNode               bool // If node should be deleted in the end
	deletePodsWithPv         bool // If pods with PersistentVolume should be deleted forcelly
	confirm                  bool // Confirm the change execution
	patchId                  string
	nodeCordonKubeletVersion string
	nodeCordonLabelsMatch    arrayFlags
}

func (c *config) Parse() {
	todaysDate := time.Now().Format("2006-01-02")

	flag.BoolVar(&c.deleteNode, "delete-nodes", false, "Delete after cleaning them")
	flag.BoolVar(&c.deletePodsWithPv, "delete-pods-with-pv", false, "Delete pods that will most probably stay stuck")
	flag.BoolVar(&c.confirm, "confirm", false, "Request to confirm before applying changes")
	flag.StringVar(&c.patchId, "patch-id", todaysDate, "Patch ID")
	flag.StringVar(&c.nodeCordonKubeletVersion, "node-version", "", "Kubelet version to target")
	flag.Var(&c.nodeCordonLabelsMatch, "node-label", "Node target label")
	flag.Parse()
}

func (config *config) wantsToDoNodeCordoning() bool {
	return len(config.nodeCordonLabelsMatch) > 0 || config.nodeCordonKubeletVersion != ""
}

type app struct {
	config config
	client *kubernetes.Clientset
	log    zap.SugaredLogger
}

func (app *app) initLogging() error {
	logger, err := zap.NewDevelopment()
	if err == nil {
		app.log = *logger.Sugar()
	}
	return err
}

func (app *app) initClient() error {
	kubeConfig := filepath.Join(os.Getenv("HOME"), ".kube", "config")
	config, err := clientcmd.BuildConfigFromFlags("", kubeConfig)

	if err != nil {
		return err
	}

	app.client, err = kubernetes.NewForConfig(config)
	return err
}

func (app *app) movePodsAwayFromCordonnedNodes() (int, error) {
	nodesList, err := app.client.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{
		FieldSelector: "spec.unschedulable=true",
	})

	if err != nil {
		return 0, err
	}

	nbMovedPods := 0

	for _, node := range nodesList.Items {
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
