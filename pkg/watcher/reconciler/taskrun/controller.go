package taskrun

import (
	"context"

	"github.com/tektoncd/pipeline/pkg/apis/pipeline"
	pipelineclient "github.com/tektoncd/pipeline/pkg/client/injection/client"
	taskruninformer "github.com/tektoncd/pipeline/pkg/client/injection/informers/pipeline/v1beta1/taskrun"
	pb "github.com/tektoncd/results/proto/v1alpha1/results_go_proto"
	"k8s.io/client-go/tools/cache"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
)

// NewController creates a Controller with provided context and configmap
func NewController(ctx context.Context, cmw configmap.Watcher, client pb.ResultsClient) *controller.Impl {
	logger := logging.FromContext(ctx)
	taskRunInformer := taskruninformer.Get(ctx)
	pipelineclientset := pipelineclient.Get(ctx)
	c := &Reconciler{
		logger:            logger,
		client:            client,
		pipelineclientset: pipelineclientset,
	}

	impl := controller.NewImpl(c, c.logger, pipeline.PipelineRunControllerName)

	taskRunInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    impl.Enqueue,
		UpdateFunc: controller.PassNew(impl.Enqueue),
	})

	return impl
}
