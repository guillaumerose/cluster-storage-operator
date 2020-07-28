package csidriveroperator

import (
	"context"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	operatorapi "github.com/openshift/api/operator/v1"
	openshiftv1 "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-storage-operator/pkg/csoclients"
	"github.com/openshift/cluster-storage-operator/pkg/generated"
	"github.com/openshift/cluster-storage-operator/pkg/operator/csidriveroperator/csioperatorclient"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/controller/manager"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/staticresourcecontroller"
	"github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/klog/v2"
)

const (
	infraConfigName = "cluster"
)

// This CSIDriverStarterController starts CSI driver controllers based on the
// underlying cloud. It does not install anything by itself, only monitors
// Infrastructure instance and starts individual ControllerManagers for the
// particular cloud. It produces following Conditions:
// CSIDriverStarterDegraded - error checking the Infrastructure
type CSIDriverStarterController struct {
	operatorClient v1helpers.OperatorClient
	infraLister    openshiftv1.InfrastructureLister
	versionGetter  status.VersionGetter
	targetVersion  string
	eventRecorder  events.Recorder

	controllers []csiDriverControllerManager
}

type csiDriverControllerManager struct {
	operatorConfig csioperatorclient.CSIOperatorConfig
	mgr            manager.ControllerManager
	running        bool
}

func NewCSIDriverStarterController(
	clients *csoclients.Clients,
	resyncInterval time.Duration,
	versionGetter status.VersionGetter,
	targetVersion string,
	eventRecorder events.Recorder,
	driverConfigs []csioperatorclient.CSIOperatorConfig) factory.Controller {
	c := &CSIDriverStarterController{
		operatorClient: clients.OperatorClient,
		infraLister:    clients.ConfigInformers.Config().V1().Infrastructures().Lister(),
		versionGetter:  versionGetter,
		targetVersion:  targetVersion,
		eventRecorder:  eventRecorder.WithComponentSuffix("CSIDriverStarter"),
	}

	// Populating all CSI driver operator ControllerManagers here simplifies
	// the startup a lot
	// - All necessary informers are populated.
	// - CSO then just Run()s
	// All ControllerManagers are not running at this point! They will be
	// started in sync() when their platform is detected.
	c.controllers = []csiDriverControllerManager{}
	for _, cfg := range driverConfigs {
		c.controllers = append(c.controllers, csiDriverControllerManager{
			operatorConfig: cfg,
			mgr:            c.createCSIControllerManager(cfg, clients, resyncInterval),
			running:        false,
		})
	}

	return factory.New().WithSync(c.sync).WithSyncDegradedOnError(clients.OperatorClient).WithInformers(
		clients.OperatorClient.Informer(),
		clients.ConfigInformers.Config().V1().Infrastructures().Informer(),
	).ToController("CSIDriverStarter", eventRecorder)
}

func (c *CSIDriverStarterController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	opSpec, _, _, err := c.operatorClient.GetOperatorState()
	if err != nil {
		return err
	}
	if opSpec.ManagementState != operatorapi.Managed {
		return nil
	}

	infrastructure, err := c.infraLister.Get(infraConfigName)
	if err != nil {
		return err
	}

	// Start controller managers for this platform
	var platform configv1.PlatformType
	if infrastructure.Status.PlatformStatus != nil {
		platform = infrastructure.Status.PlatformStatus.Type
	}

	for i := range c.controllers {
		if c.controllers[i].operatorConfig.Platform == platform && !c.controllers[i].running {
			klog.V(2).Infof("Starting CSI driver controller manager %s", c.controllers[i].operatorConfig.ConditionPrefix)
			go c.controllers[i].mgr.Start(ctx)
			c.controllers[i].running = true
		}
	}
	return nil
}

func (c *CSIDriverStarterController) createCSIControllerManager(
	cfg csioperatorclient.CSIOperatorConfig,
	clients *csoclients.Clients,
	resyncInterval time.Duration) manager.ControllerManager {

	manager := manager.NewControllerManager()
	manager = manager.WithController(staticresourcecontroller.NewStaticResourceController(
		cfg.ConditionPrefix+"CSIDriverOperatorStaticController",
		generated.Asset,
		cfg.StaticAssets,
		resourceapply.NewKubeClientHolder(clients.KubeClient),
		c.operatorClient,
		c.eventRecorder).AddKubeInformers(clients.KubeInformers), 1)

	manager = manager.WithController(NewCSIDriverOperatorCRController(
		cfg.ConditionPrefix,
		clients,
		cfg,
		c.eventRecorder,
		resyncInterval,
	), 1)

	manager = manager.WithController(NewCSIDriverOperatorDeploymentController(
		cfg.ConditionPrefix,
		clients,
		cfg,
		c.versionGetter,
		c.targetVersion,
		c.eventRecorder,
		resyncInterval,
	), 1)

	return manager
}
