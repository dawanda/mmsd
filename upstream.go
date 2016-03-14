package main

import (
	"io/ioutil"
	"log"
	"os"
	"path/filepath"

	"github.com/christianparpart/serviced/marathon"
)

type UpstreamFilesBuilder struct {
	BasePath string
}

func (upstream *UpstreamFilesBuilder) Apply(apps []*marathon.App, force bool) error {
	log.Printf("UpstreamFilesBuilder.Apply(force:%v): BasePath: %v\n", force, upstream.BasePath)

	// var newFiles []string
	oldFiles, err := upstream.collectFiles()
	if err != nil {
		return err
	}

	for i, file := range oldFiles {
		log.Printf("oldFile: %v: %v\n", i, file)
	}

	for _, app := range apps {
		app_id := PrettifyAppId(app.Id)
		cfgfile := filepath.Join(upstream.BasePath, app_id+".instances")
		tmpfile := cfgfile + ".tmp"

		os.Remove(tmpfile)
	}

	return nil
}

func (upstream *UpstreamFilesBuilder) collectFiles() ([]string, error) {
	fileInfos, err := ioutil.ReadDir(upstream.BasePath)
	if err != nil {
		return nil, err
	}

	var fileNames []string
	for _, fileInfo := range fileInfos {
		fileNames = append(fileNames, filepath.Join(upstream.BasePath, fileInfo.Name()))
	}

	return fileNames, nil
}
