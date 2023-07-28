// Copyright © 2022 Shyam Jeedigunta <shyam123.jvs95@gmail.com>
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

package cmd

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"

	"github.com/shyamjvs/kube-stress/pkg/client"
	"github.com/shyamjvs/kube-stress/pkg/util"
)

type ListConfig struct {
	namespace         string
	objectType        string
	pageSize          int
	numClients        int
	qps               float32
	totalDuration     time.Duration
	csvOutputFilepath string
}

var (
	listConfig *ListConfig
	listCmd    *cobra.Command
	csvWriter  *util.ThreadSafeCsvWriter
)

func init() {
	listConfig = &ListConfig{}
	listCmd = &cobra.Command{
		Use:   "list",
		Short: "List objects of a given type in the cluster",
		Run: func(cmd *cobra.Command, args []string) {
			if listConfig.csvOutputFilepath != "" {
				csvWriter = util.NewThreadSafeCsvWriter(listConfig.csvOutputFilepath)
				defer csvWriter.Flush()
			}
			if err := listCommand(); err != nil {
				klog.Errorf("Error executing list command: %v", err)
				os.Exit(1)
			}
		},
	}
	rootCmd.AddCommand(listCmd)
	listCmd.Flags().StringVar(&listConfig.namespace, "namespace", KubeStress, "Namespace to list the objects from (empty value means all namespaces)")
	listCmd.Flags().StringVar(&listConfig.objectType, "object-type", "configmaps", "Type of objects to create (supported values are 'pods' and 'configmaps'")
	listCmd.Flags().IntVar(&listConfig.pageSize, "page-size", 0, "Number of objects to list in a single page, i.e `limit` param (0 means no pagination)")
	listCmd.Flags().IntVar(&listConfig.numClients, "num-clients", 10, "Number of clients to use for spreading the list calls")
	listCmd.Flags().Float32Var(&listConfig.qps, "qps", 2.0, "QPS to generate for the list calls")
	listCmd.Flags().DurationVar(&listConfig.totalDuration, "total-duration", 5*time.Minute, "Total duration for which to run this command")
	listCmd.Flags().StringVar(&listConfig.csvOutputFilepath, "csv-output-filepath", "", "Path to the output CSV file where latency values will be written")
}

func listCommand() error {
	clients := client.CreateKubeClients(client.GetKubeConfig(kubeconfig), listConfig.numClients)

	// Setup signal handling for the process.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	defer once.Do(cancel)
	go func() {
		for {
			select {
			case sig := <-sigs:
				klog.V(1).Infof("Received a stop signal: %v", sig)
				once.Do(cancel)
			case <-ctx.Done():
				klog.V(1).Info("Cancelled context and exiting program")
				return
			}
		}
	}()

	klog.V(1).Infof("Listing '%v' objects in namespace '%v' (page size = %v) using %v clients and QPS = %v for %v",
		listConfig.objectType,
		listConfig.namespace,
		listConfig.pageSize,
		listConfig.numClients,
		listConfig.qps,
		listConfig.totalDuration)
	listObjects(ctx, clients)
	return nil
}

func listObjects(ctx context.Context, clients []*kubernetes.Clientset) {
	start := time.Now()
	ticker := time.NewTicker(time.Duration(1000000000.0/listConfig.qps) * time.Nanosecond)
	defer ticker.Stop()

	var totalCount atomic.Uint64
	var failedCount atomic.Uint64
	defer func() {
		fc := failedCount.Load()
		tc := totalCount.Load()
		klog.Infof("%d out of %d requests failed, failure rate: %v%%", fc, tc, float64(fc)/float64(tc)*100)
	}()

	var wg sync.WaitGroup
	for i := 0; time.Since(start) < listConfig.totalDuration; i++ {
		select {
		case <-ctx.Done():

			return
		case <-ticker.C:
			client := clients[i%len(clients)]
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := listOnce(ctx, client, &totalCount, &failedCount); err != nil {
					klog.Errorf("Error seen with list call: %v", err)
				}
			}()
		}
	}

	wg.Wait()
	klog.V(1).Infof("Finished listing objects for a duration of %v", listConfig.totalDuration)
}

func listOnce(ctx context.Context, client *kubernetes.Clientset, totalCount, failedCount *atomic.Uint64) error {
	requestCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	totalCount.Add(1)

	start := time.Now()
	rc, err := client.CoreV1().RESTClient().Get().
		Namespace(listConfig.namespace).
		Resource(listConfig.objectType).
		VersionedParams(&metav1.ListOptions{Limit: int64(listConfig.pageSize)}, scheme.ParameterCodec).
		Stream(requestCtx)
	if rc != nil {
		// Drain response.body to enable TCP connection reuse.
		// Ref: https://github.com/google/go-github/pull/317)
		io.Copy(ioutil.Discard, rc)
		if rc.Close() != nil {
			klog.Errorf("Failed to close the response: %v", err)
		}
	}
	if err != nil {
		failedCount.Add(1)
		return err
	}

	latency := time.Since(start)
	if csvWriter != nil {
		csvWriter.Write([]string{fmt.Sprintf("%v", latency)})
	}

	klog.V(2).Infof("List call took: %v", latency)
	return nil
}
