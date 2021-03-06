// Copyright (c) 2019 Palantir Technologies. All rights reserved.
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
	"time"

	clientset "github.com/palantir/k8s-spark-scheduler-lib/pkg/client/clientset/versioned"
	ssinformers "github.com/palantir/k8s-spark-scheduler-lib/pkg/client/informers/externalversions"
	"github.com/palantir/k8s-spark-scheduler/config"
	"github.com/palantir/k8s-spark-scheduler/internal/extender"
	"github.com/palantir/k8s-spark-scheduler/internal/metrics"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"
	"github.com/palantir/witchcraft-go-server/witchcraft"
	"github.com/spf13/cobra"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "runs the spark scheduler extender server",
	RunE: func(cmd *cobra.Command, args []string) error {
		return New().Start()
	},
}

func init() {
	rootCmd.AddCommand(serverCmd)
}

func initServer(ctx context.Context, info witchcraft.InitInfo) (func(), error) {
	var kubeconfig *rest.Config
	var err error

	install := info.InstallConfig.(config.Install)
	if install.Kubeconfig != "" {
		kubeconfig, err = clientcmd.BuildConfigFromFlags("", install.Kubeconfig)
		if err != nil {
			svc1log.FromContext(ctx).Error("Error building config from kubeconfig: %s", svc1log.Stacktrace(err))
			return nil, err
		}
	} else {
		kubeconfig, err = rest.InClusterConfig()
		if err != nil {
			svc1log.FromContext(ctx).Error("Error building in cluster kubeconfig: %s", svc1log.Stacktrace(err))
			return nil, err
		}
	}
	kubeconfig.QPS = install.QPS
	kubeconfig.Burst = install.Burst

	kubeClient, err := kubernetes.NewForConfig(kubeconfig)
	if err != nil {
		svc1log.FromContext(ctx).Error("Error building kubernetes clientset: %s", svc1log.Stacktrace(err))
		return nil, err
	}
	sparkSchedulerClient, err := clientset.NewForConfig(kubeconfig)
	if err != nil {
		svc1log.FromContext(ctx).Error("Error building spark scheduler clientset: %s", svc1log.Stacktrace(err))
		return nil, err
	}
	apiExtensionsClient, err := apiextensionsclientset.NewForConfig(kubeconfig)
	if err != nil {
		svc1log.FromContext(ctx).Error("Error building api extensions clientset: %s", svc1log.Stacktrace(err))
		return nil, err
	}
	err = extender.EnsureResourceReservationsCRD(apiExtensionsClient, install.ResourceReservationCRDAnnotations)
	if err != nil {
		svc1log.FromContext(ctx).Error("Error ensuring resource reservations CRD exists: %s", svc1log.Stacktrace(err))
		return nil, err
	}

	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, time.Second*30)
	sparkSchedulerInformerFactory := ssinformers.NewSharedInformerFactory(sparkSchedulerClient, time.Second*30)

	nodeInformer := kubeInformerFactory.Core().V1().Nodes()
	podInformer := kubeInformerFactory.Core().V1().Pods()
	resourceReservationInformerBeta := sparkSchedulerInformerFactory.Sparkscheduler().V1beta1().ResourceReservations()

	go kubeInformerFactory.Start(ctx.Done())
	go sparkSchedulerInformerFactory.Start(ctx.Done())
	if ok := cache.WaitForCacheSync(
		ctx.Done(),
		nodeInformer.Informer().HasSynced,
		podInformer.Informer().HasSynced,
		resourceReservationInformerBeta.Informer().HasSynced); !ok {
		svc1log.FromContext(ctx).Error("Error waiting for cache to sync")
		return nil, nil
	}

	overheadComputer := extender.NewOverheadComputer(
		ctx,
		podInformer.Lister(),
		resourceReservationInformerBeta.Lister(),
		nodeInformer.Lister(),
	)

	sparkSchedulerExtender := extender.NewExtender(
		nodeInformer.Lister(),
		extender.NewSparkPodLister(podInformer.Lister()),
		resourceReservationInformerBeta.Lister(),
		sparkSchedulerClient.SparkschedulerV1beta1(),
		kubeClient.CoreV1(),
		sparkSchedulerClient.ScalerV1alpha1(),
		apiExtensionsClient,
		install.FIFO,
		install.BinpackAlgo,
		overheadComputer,
	)

	resourceReporter := metrics.NewResourceReporter(
		nodeInformer.Lister(),
		resourceReservationInformerBeta.Lister(),
	)

	queueReporter := metrics.NewQueueReporter(podInformer.Lister())

	sparkSchedulerExtender.Start(ctx)
	go resourceReporter.StartReportingResourceUsage(ctx)
	go queueReporter.StartReportingQueues(ctx)
	go overheadComputer.Start(ctx)

	if err := registerExtenderEndpoints(info.Router, sparkSchedulerExtender); err != nil {
		return nil, err
	}

	return nil, nil
}

// New creates and returns a witchcraft Server.
func New() *witchcraft.Server {
	return witchcraft.NewServer().
		WithInstallConfigType(config.Install{}).
		WithInstallConfigFromFile("var/conf/install.yml").
		// We do this in order to get witchcraft to honor the logging config, which it expects to be in runtime
		WithRuntimeConfigFromFile("var/conf/install.yml").
		WithSelfSignedCertificate().
		WithECVKeyProvider(witchcraft.ECVKeyNoOp()).
		WithInitFunc(initServer).
		WithOrigin(svc1log.CallerPkg(0, 1))
}
