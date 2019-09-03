/*
Copyright 2017 The Fission Authors.

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

package fission_cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/dchest/uniuri"
	"github.com/fission/fission/pkg/utils"
	"github.com/hashicorp/go-multierror"
	"github.com/mholt/archiver"
	"github.com/pkg/errors"
	"github.com/satori/go.uuid"
	"github.com/urfave/cli"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fv1 "github.com/fission/fission/pkg/apis/fission.io/v1"
	"github.com/fission/fission/pkg/controller/client"
	"github.com/fission/fission/pkg/fission-cli/cliwrapper/driver/urfavecli"
	cmdutils "github.com/fission/fission/pkg/fission-cli/cmd"
	_package "github.com/fission/fission/pkg/fission-cli/cmd/package"
	"github.com/fission/fission/pkg/fission-cli/cmd/spec"
	"github.com/fission/fission/pkg/fission-cli/log"
	"github.com/fission/fission/pkg/fission-cli/util"
)

func getFunctionsByPackage(client *client.Client, pkgName, pkgNamespace string) ([]fv1.Function, error) {
	fnList, err := client.FunctionList(pkgNamespace)
	if err != nil {
		return nil, err
	}
	fns := []fv1.Function{}
	for _, fn := range fnList {
		if fn.Spec.Package.PackageRef.Name == pkgName {
			fns = append(fns, fn)
		}
	}
	return fns, nil
}

// downloadStoragesvcURL downloads and return archive content with given storage service url
func downloadStoragesvcURL(client *client.Client, fileUrl string) io.ReadCloser {
	u, err := url.ParseRequestURI(fileUrl)
	if err != nil {
		return nil
	}

	// replace in-cluster storage service host with controller server url
	fileDownloadUrl := strings.TrimSuffix(client.Url, "/") + "/proxy/storage/" + u.RequestURI()
	reader, err := _package.DownloadURL(fileDownloadUrl)

	util.CheckErr(err, fmt.Sprintf("download from storage service url: %v", fileUrl))
	return reader
}

func pkgCreate(c *cli.Context) error {
	client := util.GetApiClient(c.GlobalString("server"))

	pkgNamespace := c.String("pkgNamespace")
	envName := c.String("env")
	if len(envName) == 0 {
		log.Fatal("Need --env argument.")
	}
	envNamespace := c.String("envNamespace")
	srcArchiveFiles := c.StringSlice("src")
	deployArchiveFiles := c.StringSlice("deploy")
	buildcmd := c.String("buildcmd")

	if len(srcArchiveFiles) == 0 && len(deployArchiveFiles) == 0 {
		log.Fatal("Need --src to specify source archive, or use --deploy to specify deployment archive.")
	}

	createPackage(c, client, pkgNamespace, envName, envNamespace, srcArchiveFiles, deployArchiveFiles, buildcmd, "", "", false)

	return nil
}

func pkgUpdate(c *cli.Context) error {
	client := util.GetApiClient(c.GlobalString("server"))

	pkgName := c.String("name")
	if len(pkgName) == 0 {
		log.Fatal("Need --name argument.")
	}
	pkgNamespace := c.String("pkgNamespace")

	force := c.Bool("f")
	envName := c.String("env")
	envNamespace := c.String("envNamespace")
	srcArchiveFiles := c.StringSlice("src")
	deployArchiveFiles := c.StringSlice("deploy")
	buildcmd := c.String("buildcmd")

	if len(srcArchiveFiles) > 0 && len(deployArchiveFiles) > 0 {
		log.Fatal("Need either of --src or --deploy and not both arguments.")
	}

	if len(srcArchiveFiles) == 0 && len(deployArchiveFiles) == 0 &&
		len(envName) == 0 && len(buildcmd) == 0 {
		log.Fatal("Need --env or --src or --deploy or --buildcmd argument.")
	}

	pkg, err := client.PackageGet(&metav1.ObjectMeta{
		Namespace: pkgNamespace,
		Name:      pkgName,
	})
	util.CheckErr(err, "get package")

	// if the new env specified is the same as the old one, no need to update package
	// same is true for all update parameters, but, for now, we dont check all of them - because, its ok to
	// re-write the object with same old values, we just end up getting a new resource version for the object.
	if len(envName) > 0 && envName == pkg.Spec.Environment.Name {
		envName = ""
	}

	if envNamespace == pkg.Spec.Environment.Namespace {
		envNamespace = ""
	}

	fnList, err := getFunctionsByPackage(client, pkg.Metadata.Name, pkg.Metadata.Namespace)
	util.CheckErr(err, "get function list")

	if !force && len(fnList) > 1 {
		log.Fatal("Package is used by multiple functions, use --force to force update")
	}

	newPkgMeta, err := updatePackage(client, pkg,
		envName, envNamespace, srcArchiveFiles, deployArchiveFiles, buildcmd, false, false)
	if err != nil {
		util.CheckErr(err, "update package")
	}

	// update resource version of package reference of functions that shared the same package
	for _, fn := range fnList {
		fn.Spec.Package.PackageRef.ResourceVersion = newPkgMeta.ResourceVersion
		_, err := client.FunctionUpdate(&fn)
		util.CheckErr(err, "update function")
	}

	fmt.Printf("Package '%v' updated\n", newPkgMeta.GetName())

	return nil
}

func updatePackage(client *client.Client, pkg *fv1.Package, envName, envNamespace string,
	srcArchiveFiles []string, deployArchiveFiles []string, buildcmd string, forceRebuild bool, noZip bool) (*metav1.ObjectMeta, error) {

	var srcArchiveMetadata, deployArchiveMetadata *fv1.Archive
	needToBuild := false

	if len(envName) > 0 {
		pkg.Spec.Environment.Name = envName
		needToBuild = true
	}

	if len(envNamespace) > 0 {
		pkg.Spec.Environment.Namespace = envNamespace
		needToBuild = true
	}

	if len(buildcmd) > 0 {
		pkg.Spec.BuildCommand = buildcmd
		needToBuild = true
	}

	if len(srcArchiveFiles) > 0 {
		srcArchiveMetadata = createArchive(client, srcArchiveFiles, false, "", "")
		pkg.Spec.Source = *srcArchiveMetadata
		needToBuild = true
	}

	if len(deployArchiveFiles) > 0 {
		deployArchiveMetadata = createArchive(client, deployArchiveFiles, noZip, "", "")
		pkg.Spec.Deployment = *deployArchiveMetadata
		// Users may update the env, envNS and deploy archive at the same time,
		// but without the source archive. In this case, we should set needToBuild to false
		needToBuild = false
	}

	// Set package as pending status when needToBuild is true
	if needToBuild || forceRebuild {
		// change into pending state to trigger package build
		pkg.Status = fv1.PackageStatus{
			BuildStatus: fv1.BuildStatusPending,
		}
	}

	newPkgMeta, err := client.PackageUpdate(pkg)
	util.CheckErr(err, "update package")

	return newPkgMeta, err
}

func pkgSourceGet(c *cli.Context) error {
	client := util.GetApiClient(c.GlobalString("server"))

	pkgName := c.String("name")
	if len(pkgName) == 0 {
		log.Fatal("Need name of package, use --name")
	}
	pkgNamespace := c.String("pkgNamespace")

	output := c.String("output")

	pkg, err := client.PackageGet(&metav1.ObjectMeta{
		Namespace: pkgNamespace,
		Name:      pkgName,
	})
	if err != nil {
		return err
	}

	var reader io.Reader

	if pkg.Spec.Source.Type == fv1.ArchiveTypeLiteral {
		reader = bytes.NewReader(pkg.Spec.Source.Literal)
	} else if pkg.Spec.Source.Type == fv1.ArchiveTypeUrl {
		readCloser := downloadStoragesvcURL(client, pkg.Spec.Source.URL)
		defer readCloser.Close()
		reader = readCloser
	}

	if len(output) > 0 {
		return _package.WriteArchiveToFile(output, reader)
	} else {
		_, err := io.Copy(os.Stdout, reader)
		return err
	}
}

func pkgDeployGet(c *cli.Context) error {
	client := util.GetApiClient(c.GlobalString("server"))

	pkgName := c.String("name")
	if len(pkgName) == 0 {
		log.Fatal("Need name of package, use --name")
	}
	pkgNamespace := c.String("pkgNamespace")

	output := c.String("output")

	pkg, err := client.PackageGet(&metav1.ObjectMeta{
		Namespace: pkgNamespace,
		Name:      pkgName,
	})
	if err != nil {
		return err
	}

	var reader io.Reader

	if pkg.Spec.Deployment.Type == fv1.ArchiveTypeLiteral {
		reader = bytes.NewReader(pkg.Spec.Deployment.Literal)
	} else if pkg.Spec.Deployment.Type == fv1.ArchiveTypeUrl {
		readCloser := downloadStoragesvcURL(client, pkg.Spec.Deployment.URL)
		defer readCloser.Close()
		reader = readCloser
	}

	if len(output) > 0 {
		return _package.WriteArchiveToFile(output, reader)
	} else {
		_, err := io.Copy(os.Stdout, reader)
		return err
	}
}

func pkgInfo(c *cli.Context) error {
	client := util.GetApiClient(c.GlobalString("server"))

	pkgName := c.String("name")
	if len(pkgName) == 0 {
		log.Fatal("Need name of package, use --name")
	}
	pkgNamespace := c.String("pkgNamespace")

	pkg, err := client.PackageGet(&metav1.ObjectMeta{
		Namespace: pkgNamespace,
		Name:      pkgName,
	})
	if err != nil {
		util.CheckErr(err, fmt.Sprintf("find package %s", pkgName))
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
	fmt.Fprintf(w, "%v\t%v\n", "Name:", pkg.Metadata.Name)
	fmt.Fprintf(w, "%v\t%v\n", "Environment:", pkg.Spec.Environment.Name)
	fmt.Fprintf(w, "%v\t%v\n", "Status:", pkg.Status.BuildStatus)
	fmt.Fprintf(w, "%v\n%v", "Build Logs:", pkg.Status.BuildLog)
	w.Flush()

	return nil
}

func pkgList(c *cli.Context) error {
	client := util.GetApiClient(c.GlobalString("server"))
	// option for the user to list all orphan packages (not referenced by any function)
	listOrphans := c.Bool("orphan")
	pkgNamespace := c.String("pkgNamespace")

	pkgList, err := client.PackageList(pkgNamespace)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
	fmt.Fprintf(w, "%v\t%v\t%v\n", "NAME", "BUILD_STATUS", "ENV")
	if listOrphans {
		for _, pkg := range pkgList {
			fnList, err := getFunctionsByPackage(client, pkg.Metadata.Name, pkg.Metadata.Namespace)
			util.CheckErr(err, fmt.Sprintf("get functions sharing package %s", pkg.Metadata.Name))
			if len(fnList) == 0 {
				fmt.Fprintf(w, "%v\t%v\t%v\n", pkg.Metadata.Name, pkg.Status.BuildStatus, pkg.Spec.Environment.Name)
			}
		}
	} else {
		for _, pkg := range pkgList {
			fmt.Fprintf(w, "%v\t%v\t%v\n", pkg.Metadata.Name,
				pkg.Status.BuildStatus, pkg.Spec.Environment.Name)
		}
	}

	w.Flush()

	return nil
}

func deleteOrphanPkgs(client *client.Client, pkgNamespace string) error {
	pkgList, err := client.PackageList(pkgNamespace)
	if err != nil {
		return err
	}

	// range through all packages and find out the ones not referenced by any function
	for _, pkg := range pkgList {
		fnList, err := getFunctionsByPackage(client, pkg.Metadata.Name, pkgNamespace)
		util.CheckErr(err, fmt.Sprintf("get functions sharing package %s", pkg.Metadata.Name))
		if len(fnList) == 0 {
			err = deletePackage(client, pkg.Metadata.Name, pkgNamespace)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func deletePackage(client *client.Client, pkgName string, pkgNamespace string) error {
	return client.PackageDelete(&metav1.ObjectMeta{
		Namespace: pkgNamespace,
		Name:      pkgName,
	})
}

func pkgDelete(c *cli.Context) error {
	client := util.GetApiClient(c.GlobalString("server"))

	pkgName := c.String("name")
	pkgNamespace := c.String("pkgNamespace")
	deleteOrphans := c.Bool("orphan")

	if len(pkgName) == 0 && !deleteOrphans {
		fmt.Println("Need --name argument or --orphan flag.")
		return nil
	}
	if len(pkgName) != 0 && deleteOrphans {
		fmt.Println("Need either --name argument or --orphan flag")
		return nil
	}

	if len(pkgName) != 0 {
		force := c.Bool("f")

		_, err := client.PackageGet(&metav1.ObjectMeta{
			Namespace: pkgNamespace,
			Name:      pkgName,
		})
		util.CheckErr(err, "find package")

		fnList, err := getFunctionsByPackage(client, pkgName, pkgNamespace)
		if err != nil {
			return err
		}

		if !force && len(fnList) > 0 {
			log.Fatal("Package is used by at least one function, use -f to force delete")
		}

		err = deletePackage(client, pkgName, pkgNamespace)
		if err != nil {
			return err
		}

		fmt.Printf("Package '%v' deleted\n", pkgName)
	} else {
		err := deleteOrphanPkgs(client, pkgNamespace)
		util.CheckErr(err, "error deleting orphan packages")
		fmt.Println("Orphan packages deleted")
	}

	return nil
}

func pkgRebuild(c *cli.Context) error {
	client := util.GetApiClient(c.GlobalString("server"))

	pkgName := c.String("name")
	if len(pkgName) == 0 {
		log.Fatal("Need name of package, use --name")
	}
	pkgNamespace := c.String("pkgNamespace")

	pkg, err := client.PackageGet(&metav1.ObjectMeta{
		Name:      pkgName,
		Namespace: pkgNamespace,
	})
	util.CheckErr(err, "find package")

	if pkg.Status.BuildStatus != fv1.BuildStatusFailed {
		log.Fatal(fmt.Sprintf("Package %v is not in %v state.",
			pkg.Metadata.Name, fv1.BuildStatusFailed))
	}

	_, err = updatePackage(client, pkg, "", "", nil, nil, "", true, false)
	util.CheckErr(err, "update package")

	fmt.Printf("Retrying build for pkg %v. Use \"fission pkg info --name %v\" to view status.\n", pkg.Metadata.Name, pkg.Metadata.Name)

	return nil
}

// Return a fv1.Archive made from an archive .  If specFile, then
// create an archive upload spec in the specs directory; otherwise
// upload the archive using client.  noZip avoids zipping the
// includeFiles, but is ignored if there's more than one includeFile.
func createArchive(client *client.Client, includeFiles []string, noZip bool, specDir string, specFile string) *fv1.Archive {

	var errs *multierror.Error

	// check files existence
	for _, path := range includeFiles {
		// ignore http files
		if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
			continue
		}

		// Get files from inputs as number of files decide next steps
		files, err := utils.FindAllGlobs([]string{path})
		if err != nil {
			util.CheckErr(err, "finding all globs")
		}

		if len(files) == 0 {
			errs = multierror.Append(errs, errors.New(fmt.Sprintf("Error finding any files with path \"%v\"", path)))
		}
	}

	if errs.ErrorOrNil() != nil {
		log.Fatal(errs.Error())
	}

	if len(specFile) > 0 {
		// create an ArchiveUploadSpec and reference it from the archive
		aus := &spec.ArchiveUploadSpec{
			Name:         archiveName("", includeFiles),
			IncludeGlobs: includeFiles,
		}

		// check if this AUS exists in the specs; if so, don't create a new one
		fr, err := spec.ReadSpecs(specDir)
		util.CheckErr(err, "read specs")
		if m := fr.SpecExists(aus, false, true); m != nil {
			fmt.Printf("Re-using previously created archive %v\n", m.Name)
			aus.Name = m.Name
		} else {
			// save the uploadspec
			err := spec.SpecSave(*aus, specFile)
			util.CheckErr(err, fmt.Sprintf("write spec file %v", specFile))
		}

		// create the archive object
		ar := &fv1.Archive{
			Type: fv1.ArchiveTypeUrl,
			URL:  fmt.Sprintf("%v%v", spec.ARCHIVE_URL_PREFIX, aus.Name),
		}
		return ar
	}

	archivePath := makeArchiveFileIfNeeded("", includeFiles, noZip)

	ctx := context.Background()
	return _package.UploadArchive(ctx, client, archivePath)
}

func createPackage(c *cli.Context, client *client.Client, pkgNamespace string, envName string, envNamespace string, srcArchiveFiles []string, deployArchiveFiles []string, buildcmd string, specDir string, specFile string, noZip bool) *metav1.ObjectMeta {
	pkgSpec := fv1.PackageSpec{
		Environment: fv1.EnvironmentReference{
			Namespace: envNamespace,
			Name:      envName,
		},
	}
	var pkgStatus fv1.BuildStatus = fv1.BuildStatusSucceeded

	var pkgName string
	if len(deployArchiveFiles) > 0 {
		if len(specFile) > 0 { // we should do this in all cases, i think
			pkgStatus = fv1.BuildStatusNone
		}
		pkgSpec.Deployment = *createArchive(client, deployArchiveFiles, noZip, specDir, specFile)
		pkgName = util.KubifyName(fmt.Sprintf("%v-%v", path.Base(deployArchiveFiles[0]), uniuri.NewLen(4)))
	}
	if len(srcArchiveFiles) > 0 {
		pkgSpec.Source = *createArchive(client, srcArchiveFiles, false, specDir, specFile)
		pkgStatus = fv1.BuildStatusPending // set package build status to pending
		pkgName = util.KubifyName(fmt.Sprintf("%v-%v", path.Base(srcArchiveFiles[0]), uniuri.NewLen(4)))
	}

	if len(buildcmd) > 0 {
		pkgSpec.BuildCommand = buildcmd
	}

	if len(pkgName) == 0 {
		pkgName = strings.ToLower(uuid.NewV4().String())
	}
	pkg := &fv1.Package{
		Metadata: metav1.ObjectMeta{
			Name:      pkgName,
			Namespace: pkgNamespace,
		},
		Spec: pkgSpec,
		Status: fv1.PackageStatus{
			BuildStatus: pkgStatus,
		},
	}

	if len(specFile) > 0 {
		// if a package sith the same spec exists, don't create a new spec file
		fr, err := spec.ReadSpecs(cmdutils.GetSpecDir(urfavecli.Parse(c)))
		util.CheckErr(err, "read specs")
		if m := fr.SpecExists(pkg, false, true); m != nil {
			fmt.Printf("Re-using previously created package %v\n", m.Name)
			return m
		}

		err = spec.SpecSave(*pkg, specFile)
		util.CheckErr(err, "save package spec")
		return &pkg.Metadata
	} else {
		pkgMetadata, err := client.PackageCreate(pkg)
		util.CheckErr(err, "create package")
		fmt.Printf("Package '%v' created\n", pkgMetadata.GetName())
		return pkgMetadata
	}
}

// Create an archive from the given list of input files, unless that
// list has only one item and that item is either a zip file or a URL.
//
// If the inputs have only one file and noZip is true, the file is
// returned as-is with no zipping.  (This is used for compatibility
// with v1 envs.)  noZip is IGNORED if there is more than one input
// file.
func makeArchiveFileIfNeeded(archiveNameHint string, archiveInput []string, noZip bool) string {

	// Unique name for the archive
	archiveName := archiveName(archiveNameHint, archiveInput)

	// Get files from inputs as number of files decide next steps
	files, err := utils.FindAllGlobs(archiveInput)
	if err != nil {
		util.CheckErr(err, "finding all globs")
	}

	// We have one file; if it's a zip file or a URL, no need to archive it
	if len(files) == 1 {
		// make sure it exists
		if _, err := os.Stat(files[0]); err != nil {
			util.CheckErr(err, fmt.Sprintf("open input file %v", files[0]))
		}

		// if it's an existing zip file OR we're not supposed to zip it, don't do anything
		if archiver.Zip.Match(files[0]) || noZip {
			return files[0]
		}

		// if it's an HTTP URL, just use the URL.
		if strings.HasPrefix(files[0], "http://") || strings.HasPrefix(files[0], "https://") {
			return files[0]
		}
	}

	// For anything else, create a new archive
	tmpDir, err := utils.GetTempDir()
	if err != nil {
		util.CheckErr(err, "create temporary archive directory")
	}

	archivePath, err := utils.MakeArchive(filepath.Join(tmpDir, archiveName), archiveInput...)
	if err != nil {
		util.CheckErr(err, "create archive file")
	}

	return archivePath
}

// Name an archive
func archiveName(givenNameHint string, includedFiles []string) string {
	if len(givenNameHint) > 0 {
		return fmt.Sprintf("%v-%v", givenNameHint, uniuri.NewLen(4))
	}
	if len(includedFiles) == 0 {
		return uniuri.NewLen(8)
	}
	return fmt.Sprintf("%v-%v", util.KubifyName(includedFiles[0]), uniuri.NewLen(4))
}
