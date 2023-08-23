// Copyright 2023 Upbound Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package space

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/pterm/pterm"
	"golang.org/x/exp/maps"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/upbound/up/cmd/up/space/defaults"
	"github.com/upbound/up/cmd/up/space/prerequisites"
	"github.com/upbound/up/internal/config"
	"github.com/upbound/up/internal/install"
	"github.com/upbound/up/internal/install/helm"
	"github.com/upbound/up/internal/kube"
	"github.com/upbound/up/internal/resources"
	"github.com/upbound/up/internal/upbound"
	"github.com/upbound/up/internal/upterm"
)

const (
	hcGroup          = "internal.spaces.upbound.io"
	hcVersion        = "v1alpha1"
	hcKind           = "XHostCluster"
	hcResourcePlural = "xhostclusters"
)

var (
	watcherTimeout int64 = 600

	hostclusterGVR = schema.GroupVersionResource{
		Group:    hcGroup,
		Version:  hcVersion,
		Resource: hcResourcePlural,
	}
)

const (
	defaultTimeout = 30 * time.Second

	defaultImagePullSecret = "upbound-pull-secret"
	ns                     = "upbound-system"

	jsonKey = "_json_key"

	errReadTokenFile          = "unable to read token file"
	errReadParametersFile     = "unable to read parameters file"
	errParseInstallParameters = "unable to parse install parameters"
	errGetRegistryToken       = "failed to acquire auth token"
	errGetAccessKey           = "failed to acquire access key"
	errCreateImagePullSecret  = "failed to create image pull secret"
	errCreateLicenseSecret    = "failed to create license secret"
	errTimoutExternalIP       = "timed out waiting for externalIP to resolve"
	errUpdateConfig           = "unable to update config"

	errFmtCreateNamespace = "failed to create namespace %s"
)

// initCmd installs Upbound Spaces.
type initCmd struct {
	Kube     kubeFlags               `embed:""`
	Registry authorizedRegistryFlags `embed:""`
	install.CommonParams
	Upbound upbound.Flags `embed:""`

	Version       string `arg:"" help:"Upbound Spaces version to install."`
	Yes           bool   `name:"yes" type:"bool" help:"Answer yes to all questions"`
	PublicIngress bool   `name:"public-ingress" type:"bool" help:"For AKS,EKS,GKE expose ingress publically"`

	helmMgr    install.Manager
	prereqs    *prerequisites.Manager
	parser     install.ParameterParser
	kClient    kubernetes.Interface
	dClient    dynamic.Interface
	pullSecret *kube.ImagePullApplicator
	quiet      config.QuietFlag
}

func init() {
	// NOTE(tnthornton) we override the runtime.ErrorHandlers so that Helm
	// doesn't leak Println logs.
	runtime.ErrorHandlers = []func(error){} //nolint:reassign
}

// BeforeApply sets default values in login before assignment and validation.
func (c *initCmd) BeforeApply() error {
	c.Set = make(map[string]string)
	return nil
}

// AfterApply sets default values in command after assignment and validation.
func (c *initCmd) AfterApply(kongCtx *kong.Context, quiet config.QuietFlag) error { //nolint:gocyclo
	if err := c.Kube.AfterApply(); err != nil {
		return err
	}
	if err := c.Registry.AfterApply(); err != nil {
		return err
	}

	// NOTE(tnthornton) we currently only have support for stylized output.
	pterm.EnableStyling()
	upterm.DefaultObjPrinter.Pretty = true

	upCtx, err := upbound.NewFromFlags(c.Upbound)
	if err != nil {
		return err
	}
	kongCtx.Bind(upCtx)

	kClient, err := kubernetes.NewForConfig(c.Kube.config)
	if err != nil {
		return err
	}
	c.kClient = kClient

	// set the defaults
	cloud := c.Set[defaults.ClusterTypeStr]
	defs, err := defaults.GetConfig(c.kClient, cloud)
	if err != nil {
		return err
	}
	// User supplied values always override the defaults
	maps.Copy(defs.SpacesValues, c.Set)
	c.Set = defs.SpacesValues
	if !c.PublicIngress {
		defs.PublicIngress = false
	} else {
		pterm.Info.Println("Public ingress will be exposed")
	}

	prereqs, err := prerequisites.New(c.Kube.config, defs)
	if err != nil {
		return err
	}
	c.prereqs = prereqs

	secret := kube.NewSecretApplicator(kClient)
	c.pullSecret = kube.NewImagePullApplicator(secret)
	dClient, err := dynamic.NewForConfig(c.Kube.config)
	if err != nil {
		return err
	}
	c.dClient = dClient
	mgr, err := helm.NewManager(c.Kube.config,
		spacesChart,
		c.Registry.Repository,
		helm.WithNamespace(ns),
		helm.WithBasicAuth(c.Registry.Username, c.Registry.Password),
		helm.IsOCI(),
		helm.WithChart(c.Bundle),
		helm.Wait(),
	)
	if err != nil {
		return err
	}
	c.helmMgr = mgr

	base := map[string]any{}
	if c.File != nil {
		defer c.File.Close() //nolint:errcheck,gosec
		b, err := io.ReadAll(c.File)
		if err != nil {
			return errors.Wrap(err, errReadParametersFile)
		}
		if err := yaml.Unmarshal(b, &base); err != nil {
			return errors.Wrap(err, errReadParametersFile)
		}
		if err := c.File.Close(); err != nil {
			return errors.Wrap(err, errReadParametersFile)
		}
	}
	c.parser = helm.NewParser(base, c.Set)
	c.quiet = quiet

	return nil
}

// Run executes the install command.
func (c *initCmd) Run() error {
	ctx := context.Background()

	params, err := c.parser.Parse()
	if err != nil {
		return errors.Wrap(err, errParseInstallParameters)
	}
	overrideRegistry(c.Registry.Repository.String(), params)

	// check if required prerequisites are installed
	status := c.prereqs.Check()

	// At least 1 prerequisite is not installed, check if we should install the
	// missing ones for the client.
	if len(status.NotInstalled) > 0 {
		pterm.Warning.Printfln("One or more required prerequisites are not installed:")
		pterm.Println()
		for _, p := range status.NotInstalled {
			pterm.Println(fmt.Sprintf("❌ %s", p.GetName()))
		}

		if !c.Yes {
			pterm.DefaultInteractiveConfirm.DefaultText = "Would you like to install them now?"
			pterm.Println() // Blank line
			result, _ := pterm.DefaultInteractiveConfirm.Show()
			pterm.Println() // Blank line
			if !result {
				pterm.Error.Println("prerequisites must be met in order to proceed with installation")
				return nil
			}
		}
		if err := c.installPrereqs(); err != nil {
			return err
		}
	}

	pterm.Info.Printfln("Required prerequisites met!")
	pterm.Info.Printfln("Proceeding with Upbound Spaces installation...")

	if err := c.applySecret(ctx, &c.Registry, ns); err != nil {
		return err
	}

	if err := c.deploySpace(context.Background(), params); err != nil {
		return err
	}

	pterm.Info.WithPrefix(upterm.RaisedPrefix).Println("Your Upbound Space is Ready!")

	outputNextSteps()
	return nil
}

func (c *initCmd) installPrereqs() error {
	status := c.prereqs.Check()
	for i, p := range status.NotInstalled {
		if err := upterm.WrapWithSuccessSpinner(
			upterm.StepCounter(
				fmt.Sprintf("Installing %s", p.GetName()),
				i+1,
				len(status.NotInstalled),
			),
			upterm.CheckmarkSuccessSpinner,
			p.Install,
		); err != nil {
			return err
		}
	}
	return nil
}

func (c *initCmd) applySecret(ctx context.Context, regFlags *authorizedRegistryFlags, namespace string) error {
	creatPullSecret := func() error {
		if err := c.pullSecret.Apply(
			ctx,
			defaultImagePullSecret,
			namespace,
			regFlags.Username,
			regFlags.Password,
			regFlags.Endpoint.String(),
		); err != nil {
			return errors.Wrap(err, errCreateImagePullSecret)
		}
		return nil
	}

	_, err := c.kClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	}, metav1.CreateOptions{})
	if err != nil && !kerrors.IsAlreadyExists(err) {
		return errors.Wrap(err, fmt.Sprintf(errFmtCreateNamespace, ns))
	}

	if err := upterm.WrapWithSuccessSpinner(
		upterm.StepCounter(fmt.Sprintf("Creating pull secret %s", defaultImagePullSecret), 1, 3),
		upterm.CheckmarkSuccessSpinner,
		creatPullSecret,
	); err != nil {
		return err
	}
	return nil
}

func (c *initCmd) deploySpace(ctx context.Context, params map[string]any) error {
	install := func() error {
		if err := c.helmMgr.Install(strings.TrimPrefix(c.Version, "v"), params); err != nil {
			return err
		}
		return nil
	}

	if c.quiet {
		return install()
	}

	if err := upterm.WrapWithSuccessSpinner(
		upterm.StepCounter("Initializing Space components", 2, 3),
		upterm.CheckmarkSuccessSpinner,
		install,
	); err != nil {
		return err
	}

	hcSpinner, _ := upterm.CheckmarkSuccessSpinner.Start(upterm.StepCounter("Starting Space Components", 3, 3))

	errC, err := kube.DynamicWatch(ctx, c.dClient.Resource(hostclusterGVR), &watcherTimeout, func(u *unstructured.Unstructured) (bool, error) {
		up := resources.HostCluster{Unstructured: *u}
		if resource.IsConditionTrue(up.GetCondition(xpv1.TypeReady)) {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return err
	}
	if err := <-errC; err != nil {
		return err
	}
	hcSpinner.Success()
	return nil
}

func outputNextSteps() {
	pterm.Println()
	pterm.Info.WithPrefix(upterm.EyesPrefix).Println("Next Steps 👇")
	pterm.Println()
	pterm.Println("👉 Check out Upbound Spaces docs @ https://docs.upbound.io/concepts/upbound-spaces")
}
