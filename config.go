package main

import (
	"flag"
	"fmt"
	"time"
)

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
	deletePodsWithPV         bool // If pods with PersistentVolume should be deleted forcelly
	confirm                  bool // Confirm the change execution
	patchID                  string
	nodeCordonKubeletVersion string
	nodeCordonLabelsMatch    arrayFlags
}

func (c *config) Parse() {
	todaysDate := time.Now().Format("2006-01-02")

	flag.BoolVar(&c.deleteNode, "delete-nodes", false, "Delete after cleaning them")
	flag.BoolVar(&c.deletePodsWithPV, "delete-pods-with-pv", false, "Delete pods with persistent volumes")
	flag.BoolVar(&c.confirm, "confirm", false, "Request to confirm before applying changes")
	flag.StringVar(&c.patchID, "patch-id", todaysDate, "Patch ID")
	flag.StringVar(&c.nodeCordonKubeletVersion, "node-version", "", "Kubelet version to target")
	flag.Var(&c.nodeCordonLabelsMatch, "node-label", "Node target label")
	flag.Parse()
}

func (c *config) wantsToDoNodeCordoning() bool {
	return len(c.nodeCordonLabelsMatch) > 0 || c.nodeCordonKubeletVersion != ""
}
