/*
Copyright 2017 The Kubernetes Authors.

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

package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/git"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/logrusutil"
	"k8s.io/test-infra/prow/tide"
)

var (
	port = flag.Int("port", 8888, "Port to listen on.")

	dryRun  = flag.Bool("dry-run", true, "Whether to mutate any real-world state.")
	runOnce = flag.Bool("run-once", false, "If true, run only once then quit.")
	deckURL = flag.String("deck-url", "", "Deck URL for read-only access to the cluster.")

	configPath = flag.String("config-path", "/etc/config/config", "Path to config.yaml.")
	cluster    = flag.String("cluster", "", "Path to kube.Cluster YAML file. If empty, uses the local cluster.")

	githubEndpoint  = flag.String("github-endpoint", "https://api.github.com", "GitHub's API endpoint.")
	githubTokenFile = flag.String("github-token-file", "/etc/github/oauth", "Path to the file containing the GitHub OAuth token.")
)

func main() {
	flag.Parse()
	logrus.SetFormatter(
		logrusutil.NewDefaultFieldsFormatter(nil, logrus.Fields{"component": "tide"}),
	)

	configAgent := &config.Agent{}
	if err := configAgent.Start(*configPath); err != nil {
		logrus.WithError(err).Fatal("Error starting config agent.")
	}

	oauthSecretRaw, err := ioutil.ReadFile(*githubTokenFile)
	if err != nil {
		logrus.WithError(err).Fatalf("Could not read oauth secret file.")
	}
	oauthSecret := string(bytes.TrimSpace(oauthSecretRaw))

	_, err = url.Parse(*githubEndpoint)
	if err != nil {
		logrus.WithError(err).Fatalf("Must specify a valid --github-endpoint URL.")
	}

	var ghcSync, ghcStatus *github.Client
	var kc *kube.Client
	if *dryRun {
		ghcSync = github.NewDryRunClient(oauthSecret, *githubEndpoint)
		ghcStatus = github.NewDryRunClient(oauthSecret, *githubEndpoint)
		kc = kube.NewFakeClient(*deckURL)
	} else {
		ghcSync = github.NewClient(oauthSecret, *githubEndpoint)
		ghcStatus = github.NewClient(oauthSecret, *githubEndpoint)
		if *cluster == "" {
			kc, err = kube.NewClientInCluster(configAgent.Config().ProwJobNamespace)
			if err != nil {
				logrus.WithError(err).Fatal("Error getting kube client.")
			}
		} else {
			kc, err = kube.NewClientFromFile(*cluster, configAgent.Config().ProwJobNamespace)
			if err != nil {
				logrus.WithError(err).Fatal("Error getting kube client.")
			}
		}
	}
	// The sync loop should be allowed more tokens than the status loop because
	// it has to list all PRs in the pool every loop while the status loop only
	// has to list changed PRs every loop.
	// The sync loop should have a much lower burst allowance than the status
	// loop which may need to update many statuses upon restarting Tide after
	// changing the context format or starting Tide on a new repo.
	ghcSync.Throttle(800, 20)
	ghcStatus.Throttle(400, 200)

	gc, err := git.NewClient()
	if err != nil {
		logrus.WithError(err).Fatal("Error getting git client.")
	}
	defer gc.Clean()

	c := tide.NewController(ghcSync, ghcStatus, kc, configAgent, gc, nil)

	start := time.Now()
	sync(c)
	if *runOnce {
		return
	}
	go func() {
		for {
			time.Sleep(time.Until(start.Add(configAgent.Config().Tide.SyncPeriod)))
			start = time.Now()
			sync(c)
		}
	}()
	logrus.Fatal(http.ListenAndServe(":"+strconv.Itoa(*port), c))
}

func sync(c *tide.Controller) {
	start := time.Now()
	if err := c.Sync(); err != nil {
		logrus.WithError(err).Error("Error syncing.")
	}
	logrus.WithField("duration", fmt.Sprintf("%v", time.Since(start))).Info("Synced")
}
