// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command buildcop searches for sponge_log.xml files and publishes them to
// Pub/Sub.
//
// You can run it locally by running:
//   go build
//   ./buildcop -repo=my-org/my-repo -installation_id=123 -project=my-project
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"

	"cloud.google.com/go/pubsub"
	"google.golang.org/api/option"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("[Buildcop] ")
	log.SetOutput(os.Stderr)

	repo := flag.String("repo", "", "The repo this is for. Defaults to auto-detect from Kokoro environment. If that doesn't work, if your repo is github.com/GoogleCloudPlatform/golang-samples, --repo should be GoogleCloudPlatform/golang-samples")
	installationID := flag.String("installation_id", "", "GitHub installation ID. Defaults to auto-detect. If your repo is not part of GoogleCloudPlatform or googleapis set this to the GitHub installation ID for your repo. See https://github.com/googleapis/repo-automation-bots/issues.")
	projectID := flag.String("project", "repo-automation-bots", "Project ID to publish to. Defaults to repo-automation-bots.")
	topicID := flag.String("topic", "passthrough", "Pub/Sub topic to publish to. Defaults to passthrough.")
	logsDir := flag.String("logs_dir", ".", "The directory to look for logs in. Defaults to current directory.")
	commit := flag.String("commit_hash", "", "Long form commit hash this build is being run for. Defaults to the KOKORO_GIT_COMMIT environment variable.")
	serviceAccount := flag.String("service_account", "", "Path to service account to use instead of Trampoline default.")

	flag.Parse()

	log.Println("Sending logs to Build Cop Bot...")
	log.Println("See https://github.com/googleapis/repo-automation-bots/tree/master/packages/buildcop.")
	if ok := publish(*projectID, *topicID, *repo, *installationID, *commit, *logsDir, *serviceAccount); !ok {
		os.Exit(1)
	}
	log.Println("Done!")
}

type githubInstallation struct {
	ID string `json:"id"`
}

type message struct {
	Name         string             `json:"name"`
	Type         string             `json:"type"`
	Location     string             `json:"location"`
	Installation githubInstallation `json:"installation"`
	Repo         string             `json:"repo"`
	Commit       string             `json:"commit"`
	BuildURL     string             `json:"buildURL"`
	XUnitXML     string             `json:"xunitXML"`
}

// publish searches for sponge_log.xml files and publishes them to Pub/Sub.
// publish logs a message and returns false if there was an error.
func publish(projectID, topicID, repo, installationID, commit, logsDir, serviceAccount string) (ok bool) {
	ctx := context.Background()

	if serviceAccount == "" {
		gfileDir := os.Getenv("KOKORO_GFILE_DIR")
		if gfileDir == "" {
			log.Println("KOKORO_GFILE_DIR not set, unable to get service account")
			return false
		}
		serviceAccount = filepath.Join(gfileDir, "kokoro-trampoline.service-account.json")
	}

	client, err := pubsub.NewClient(ctx, projectID, option.WithCredentialsFile(serviceAccount))
	if err != nil {
		log.Printf("Unable to connect to Pub/Sub: %v", err)
		return false
	}
	topic := client.Topic(topicID)

	if repo == "" {
		repo = detectRepo()
		if repo == "" {
			log.Print(`Unable to detect repo. Please set the --repo flag.
If your repo is github.com/GoogleCloudPlatform/golang-samples, --repo should be GoogleCloudPlatform/golang-samples.

If your repo is not in GoogleCloudPlatform or googleapis, you must also set
--installation_id. See https://github.com/apps/build-cop-bot/.`)
			return false
		}
	}

	if installationID == "" {
		installationID = detectInstallationID(repo)
		if installationID == "" {
			log.Printf(`Unable to detect installation ID from repo=%q. Please set the --installation_id flag.
If your repo is part of GoogleCloudPlatform or googleapis and you see this error,
file an issue at https://github.com/googleapis/repo-automation-bots/issues.
Otherwise, set --installation_id with the numeric installation ID.
See https://github.com/apps/build-cop-bot/.`, repo)
			return false
		}
	}

	if commit == "" {
		commit = os.Getenv("KOKORO_GIT_COMMIT")
		if commit == "" {
			log.Printf(`Unable to detect commit hash (expected the KOKORO_GIT_COMMIT env var).
Please set --commit_hash to the latest git commit hash.
See https://github.com/apps/build-cop-bot/.`)
			return false
		}
	}

	// Handle logs in the current directory.
	if err := filepath.Walk(logsDir, processLog(ctx, repo, installationID, commit, topic)); err != nil {
		log.Printf("Error publishing logs: %v", err)
		return false
	}

	return true
}

// detectRepo tries to detect the repo from the environment.
func detectRepo() string {
	githubURL := os.Getenv("KOKORO_GITHUB_COMMIT_URL")
	if githubURL == "" {
		githubURL = os.Getenv("KOKORO_GITHUB_PULL_REQUEST_URL")
		if githubURL != "" {
			log.Printf("Warning! Running on a PR. Double check how you call buildocp before merging.")
		}
	}
	if githubURL == "" {
		return ""
	}
	parts := strings.Split(githubURL, "/")
	if len(parts) < 5 {
		return ""
	}
	repo := fmt.Sprintf("%s/%s", parts[3], parts[4])
	return repo
}

// detectInstallationID tries to detect the GitHub installation ID based on the
// repo.
func detectInstallationID(repo string) string {
	if strings.Contains(repo, "GoogleCloudPlatform") {
		return "5943459"
	}
	if strings.Contains(repo, "googleapis") {
		return "6370238"
	}
	return ""
}

// processLog is used to process log files and publish them to Pub/Sub.
func processLog(ctx context.Context, repo, installationID, commit string, topic *pubsub.Topic) filepath.WalkFunc {
	return func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !strings.HasSuffix(info.Name(), "sponge_log.xml") {
			return nil
		}
		log.Printf("Processing %v", path)
		data, err := ioutil.ReadFile(path)
		if err != nil {
			return fmt.Errorf("ioutil.ReadFile(%q): %v", path, err)
		}
		enc := base64.StdEncoding.EncodeToString(data)
		buildURL := fmt.Sprintf("[Build Status](https://source.cloud.google.com/results/invocations/%s), [Sponge](http://sponge2/%s)", os.Getenv("KOKORO_BUILD_ID"), os.Getenv("KOKORO_BUILD_ID"))
		msg := message{
			Name:         "buildcop",
			Type:         "function",
			Location:     "us-central1",
			Installation: githubInstallation{ID: installationID},
			Repo:         repo,
			Commit:       commit,
			BuildURL:     buildURL,
			XUnitXML:     enc,
		}
		data, err = json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("json.Marshal: %v", err)
		}
		pubsubMsg := &pubsub.Message{
			Data: data,
		}
		id, err := topic.Publish(ctx, pubsubMsg).Get(ctx)
		if err != nil {
			return fmt.Errorf("Pub/Sub Publish.Get: %v", err)
		}
		log.Printf("Success! ID=%v", id)
		return nil
	}
}
