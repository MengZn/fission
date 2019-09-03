/*
Copyright 2019 The Fission Authors.

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

package _package

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/fission/fission/pkg/utils"
	uuid "github.com/satori/go.uuid"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	fv1 "github.com/fission/fission/pkg/apis/fission.io/v1"
	"github.com/fission/fission/pkg/controller/client"
	"github.com/fission/fission/pkg/fission-cli/util"
	storageSvcClient "github.com/fission/fission/pkg/storagesvc/client"
	"github.com/fission/fission/pkg/types"
)

func FileSize(filePath string) int64 {
	info, err := os.Stat(filePath)
	util.CheckErr(err, fmt.Sprintf("stat %v", filePath))
	return info.Size()
}

func UploadArchive(ctx context.Context, client *client.Client, fileName string) *fv1.Archive {
	var archive fv1.Archive

	// If filename is a URL, download it first
	if strings.HasPrefix(fileName, "http://") || strings.HasPrefix(fileName, "https://") {
		fileName = DownloadToTempFile(fileName)
	}

	if FileSize(fileName) < types.ArchiveLiteralSizeLimit {
		archive.Type = fv1.ArchiveTypeLiteral
		archive.Literal = GetContents(fileName)
	} else {
		u := strings.TrimSuffix(client.Url, "/") + "/proxy/storage"
		ssClient := storageSvcClient.MakeClient(u)

		// TODO add a progress bar
		id, err := ssClient.Upload(ctx, fileName, nil)
		util.CheckErr(err, fmt.Sprintf("upload file %v", fileName))

		storageSvc, err := client.GetSvcURL("application=fission-storage")
		storageSvcURL := "http://" + storageSvc
		util.CheckErr(err, "get fission storage service name")

		// We make a new client with actual URL of Storage service so that the URL is not
		// pointing to 127.0.0.1 i.e. proxy. DON'T reuse previous ssClient
		pkgClient := storageSvcClient.MakeClient(storageSvcURL)
		archiveURL := pkgClient.GetUrl(id)

		archive.Type = fv1.ArchiveTypeUrl
		archive.URL = archiveURL

		csum, err := FileChecksum(fileName)
		util.CheckErr(err, fmt.Sprintf("calculate checksum for file %v", fileName))

		archive.Checksum = *csum
	}
	return &archive
}

func GetContents(filePath string) []byte {
	var code []byte
	var err error

	code, err = ioutil.ReadFile(filePath)
	util.CheckErr(err, fmt.Sprintf("read %v", filePath))
	return code
}

func FileChecksum(fileName string) (*fv1.Checksum, error) {
	f, err := os.Open(fileName)
	if err != nil {
		return nil, fmt.Errorf("failed to open file %v: %v", fileName, err)
	}
	defer f.Close()

	h := sha256.New()
	_, err = io.Copy(h, f)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate checksum for %v", fileName)
	}

	return &fv1.Checksum{
		Type: fv1.ChecksumTypeSHA256,
		Sum:  hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// DownloadToTempFile fetches archive file from arbitrary url
// and write it to temp file for further usage
func DownloadToTempFile(fileUrl string) string {
	reader, err := DownloadURL(fileUrl)
	util.CheckErr(err, fmt.Sprintf("download from url: %v", fileUrl))
	defer reader.Close()

	tmpDir, err := utils.GetTempDir()
	util.CheckErr(err, "create temp directory")

	tmpFilename := uuid.NewV4().String()
	destination := filepath.Join(tmpDir, tmpFilename)
	err = os.Mkdir(tmpDir, 0744)
	util.CheckErr(err, "create temp directory")

	err = WriteArchiveToFile(destination, reader)
	util.CheckErr(err, "write archive to file")

	return destination
}

// DownloadURL downloads file from given url
func DownloadURL(fileUrl string) (io.ReadCloser, error) {
	resp, err := http.Get(fileUrl)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%v - HTTP response returned non 200 status", resp.StatusCode)
	}
	return resp.Body, nil
}

func WriteArchiveToFile(fileName string, reader io.Reader) error {
	tmpDir, err := utils.GetTempDir()
	if err != nil {
		return err
	}

	path := filepath.Join(tmpDir, fileName+".tmp")
	w, err := os.Create(path)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, reader)
	if err != nil {
		return err
	}
	err = os.Chmod(path, 0644)
	if err != nil {
		return err
	}

	err = os.Rename(path, fileName)
	if err != nil {
		return err
	}

	return nil
}
