// Copyright 2024 Upbound Inc
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

package exporter

import (
	"context"
	"fmt"
	"strings"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/mholt/archiver/v4"
	"github.com/pterm/pterm"
	"github.com/spf13/afero"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/meta"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	appsv1 "k8s.io/client-go/kubernetes/typed/apps/v1"

	"github.com/upbound/up/internal/migration/meta/v1alpha1"
	"github.com/upbound/up/internal/upterm"
)

// Options for the exporter.
type Options struct {
	// OutputArchive is the path to the archive file to be created.
	OutputArchive string
}

// ControlPlaneStateExporter exports the state of a Crossplane control plane.
type ControlPlaneStateExporter struct {
	crdClient       apiextensionsclientset.Interface
	dynamicClient   dynamic.Interface
	discoveryClient discovery.DiscoveryInterface
	appsClient      appsv1.AppsV1Interface
	resourceMapper  meta.RESTMapper
	options         Options
}

// NewControlPlaneStateExporter returns a new ControlPlaneStateExporter.
func NewControlPlaneStateExporter(crdClient apiextensionsclientset.Interface, dynamicClient dynamic.Interface, discoveryClient discovery.DiscoveryInterface, appsClient appsv1.AppsV1Interface, mapper meta.RESTMapper, opts Options) *ControlPlaneStateExporter {
	return &ControlPlaneStateExporter{
		crdClient:       crdClient,
		dynamicClient:   dynamicClient,
		discoveryClient: discoveryClient,
		appsClient:      appsClient,
		resourceMapper:  mapper,
		options:         opts,
	}
}

// Export exports the state of the control plane.
func (e *ControlPlaneStateExporter) Export(ctx context.Context) error { // nolint:gocyclo
	pterm.EnableStyling()
	upterm.DefaultObjPrinter.Pretty = true

	pterm.Println("Starting SOS Report...")

	fs := afero.Afero{Fs: afero.NewOsFs()}
	tmpDir, err := fs.TempDir("", "up")
	if err != nil {
		return errors.Wrap(err, "cannot create temporary directory")
	}
	defer func() {
		_ = fs.RemoveAll(tmpDir)
	}()

	// Scan the control plane for types to export.
	scanMsg := "Scanning control plane... "
	s, _ := upterm.CheckmarkSuccessSpinner.Start(scanMsg)
	crdList, err := fetchAllCRDs(ctx, e.crdClient)
	if err != nil {
		s.Fail(scanMsg + "Failed!")
		return errors.Wrap(err, "cannot fetch CRDs")
	}
	exportList := make([]apiextensionsv1.CustomResourceDefinition, 0, len(crdList))
	for _, crd := range crdList {
		// We only want to export the following types:
		// - Crossplane Core CRDs - Has suffix ".crossplane.io".
		// - CRDs owned by Crossplane packages - Has owner reference to a Crossplane package.
		// - CRDs owned by a CompositeResourceDefinition - Has owner reference to a CompositeResourceDefinition.
		if !e.shouldExport(crd) {
			// Ignore CRDs that we don't want to export.
			continue
		}
		exportList = append(exportList, crd)
	}
	s.Success(scanMsg + fmt.Sprintf("%d types found! 👀", len(exportList)))

	// Export Crossplane resources.
	crCounts := make(map[string]int, len(exportList))
	exportCRsMsg := "Exporting sos report resources... "
	s, _ = upterm.CheckmarkSuccessSpinner.Start(exportCRsMsg + fmt.Sprintf("0 / %d", len(exportList)))
	for _, crd := range exportList {
		gvr, err := e.customResourceGVR(crd)
		if err != nil {
			s.Fail(exportCRsMsg + "Failed!")
			return errors.Wrapf(err, "cannot get GVR for %q", crd.GetName())
		}

		s.UpdateText(exportCRsMsg + fmt.Sprintf("Analyse %s...", gvr.GroupResource()))

		sub := false
		for _, vr := range crd.Spec.Versions {
			if vr.Storage && vr.Subresources != nil && vr.Subresources.Status != nil {
				// This CRD has a status subresource. We store this as a metadata per type and use
				// it during import to determine if we should apply the status subresource.
				sub = true
				break
			}
		}
		exporter := NewUnstructuredExporter(
			NewUnstructuredFetcher(e.dynamicClient, e.options),
			NewFileSystemPersister(fs, tmpDir, &v1alpha1.TypeMeta{
				Categories:            crd.Spec.Names.Categories,
				WithStatusSubresource: sub,
			}))

		// ExportResource will fetch all resources of the given GVR and store them in the
		// well-known directory structure.
		count, err := exporter.ExportResources(ctx, gvr)
		if err != nil {
			s.Fail(exportCRsMsg + "Failed!")
			return errors.Wrapf(err, "cannot export resources for %q", crd.GetName())
		}
		crCounts[gvr.GroupResource().String()] = count
	}

	total := 0
	for _, count := range crCounts {
		total += count
	}
	s.Success(exportCRsMsg + " exported! 📤")

	// Export a top level metadata file. This file contains details like when the export was done,
	// the version and feature flags of Crossplane and number of resources exported per type.
	// This metadata file is used during import to determine if the import is compatible with the
	// current Crossplane version and feature flags and also enables manual inspection the exported state.
	me := NewPersistentMetadataExporter(e.appsClient, e.dynamicClient, fs, tmpDir)
	if err = me.ExportMetadata(ctx, e.options, crCounts); err != nil {
		return errors.Wrap(err, "cannot write export metadata")
	}

	// Archive the sos report state.
	archiveMsg := "Report state... "
	s, _ = upterm.CheckmarkSuccessSpinner.Start(archiveMsg)
	if err = e.archive(ctx, fs, tmpDir); err != nil {
		s.Fail(archiveMsg + "Failed!")
		return errors.Wrap(err, "cannot archive exported state")
	}
	s.Success(archiveMsg + fmt.Sprintf("archived to %q! 📦", e.options.OutputArchive))

	pterm.Println("\nSuccessfully exported sos report!")
	return nil
}

func (e *ControlPlaneStateExporter) shouldExport(in apiextensionsv1.CustomResourceDefinition) bool {
	for _, ref := range in.GetOwnerReferences() {
		// Types owned by a Crossplane package.
		if ref.APIVersion == "pkg.crossplane.io/v1" {
			return true
		}
	}

	return strings.HasSuffix(in.GetName(), ".crossplane.io")
}

func (e *ControlPlaneStateExporter) customResourceGVR(in apiextensionsv1.CustomResourceDefinition) (schema.GroupVersionResource, error) {
	version := ""
	for _, vr := range in.Spec.Versions {
		if vr.Storage {
			version = vr.Name
		}
	}

	rm, err := e.resourceMapper.RESTMapping(schema.GroupKind{
		Group: in.Spec.Group,
		Kind:  in.Spec.Names.Kind,
	}, version)

	if err != nil {
		return schema.GroupVersionResource{}, errors.Wrapf(err, "cannot get REST mapping for %q", in.GetName())
	}

	return rm.Resource, nil
}

func (e *ControlPlaneStateExporter) archive(ctx context.Context, fs afero.Afero, dir string) error {
	files, err := archiver.FilesFromDisk(nil, map[string]string{
		dir + "/": "",
	})
	if err != nil {
		return err
	}

	out, err := fs.Create(e.options.OutputArchive)
	if err != nil {
		return err
	}
	defer func() {
		_ = out.Close()
	}()

	if err = fs.Chmod(e.options.OutputArchive, 0600); err != nil {
		return err
	}

	format := archiver.CompressedArchive{
		Compression: archiver.Gz{},
		Archival:    archiver.Tar{},
	}

	return format.Archive(ctx, out, files)
}

func fetchAllCRDs(ctx context.Context, kube apiextensionsclientset.Interface) ([]apiextensionsv1.CustomResourceDefinition, error) {
	var crds []apiextensionsv1.CustomResourceDefinition

	continueToken := ""
	for {
		l, err := kube.ApiextensionsV1().CustomResourceDefinitions().List(ctx, v1.ListOptions{
			Limit:    defaultPageSize,
			Continue: continueToken,
		})
		if err != nil {
			return nil, errors.Wrap(err, "cannot list CRDs")
		}
		crds = append(crds, l.Items...)
		continueToken = l.GetContinue()
		if continueToken == "" {
			break
		}
	}

	return crds, nil
}
