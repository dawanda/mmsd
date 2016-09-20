package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/dawanda/mmsd/module_api"
)

type FilesManager struct {
	Verbose  bool
	BasePath string
}

func (upstream *FilesManager) Log(msg string) {
	if upstream.Verbose {
		log.Printf("upstream: %v\n", msg)
	}
}

func (manager *FilesManager) Startup() {
}

func (manager *FilesManager) Shutdown() {
}

func (manager *FilesManager) RemoveTask(task *module_api.AppBackend, app *module_api.AppCluster) {
	if app != nil {
		manager.writeApp(app)
	} else {
		// TODO: remove files for app-$portIndex
	}
}

func (upstream *FilesManager) AddTask(task *module_api.AppBackend, app *module_api.AppCluster) {
	upstream.writeApp(app)
}

func (upstream *FilesManager) Apply(apps []*module_api.AppCluster) {
	err := os.MkdirAll(upstream.BasePath, 0770)
	if err != nil {
		log.Printf("Failed to mkdir. %v", err)
		return
	}

	var newFiles []string
	oldFiles, err := upstream.collectFiles()
	if err != nil {
		log.Printf("Failed to collect files. %v", err)
		return
	}

	for _, app := range apps {
		filenames, _ := upstream.writeApp(app)
		newFiles = append(newFiles, filenames...)
	}

	// check for superfluous files
	diff := FindMissing(oldFiles, newFiles)
	for _, superfluous := range diff {
		upstream.Log(fmt.Sprintf("Removing superfluous file: %v\n", superfluous))
		os.Remove(superfluous)
	}
}

func (upstream *FilesManager) writeApp(app *module_api.AppCluster) ([]string, error) {
	var files []string

	app_id := app.Id
	cfgfile := filepath.Join(upstream.BasePath, app_id+".instances")
	tmpfile := cfgfile + ".tmp"

	err := upstream.writeFile(tmpfile, app_id, app)
	if err != nil {
		return files, err
	}
	files = append(files, cfgfile)

	if _, err := os.Stat(cfgfile); os.IsNotExist(err) {
		upstream.Log(fmt.Sprintf("new %v", cfgfile))
		os.Rename(tmpfile, cfgfile)
	} else if !FileIsIdentical(tmpfile, cfgfile) {
		upstream.Log(fmt.Sprintf("refresh %v", cfgfile))
		os.Rename(tmpfile, cfgfile)
	} else {
		// new file is identical to already existing one
		os.Remove(tmpfile)
	}
	return files, nil
}

func (upstream *FilesManager) writeFile(filename string, appId string,
	app *module_api.AppCluster) error {

	var b bytes.Buffer
	b.WriteString(fmt.Sprintf("Service-Name: %v\r\n", appId))
	b.WriteString(fmt.Sprintf("Service-Port: %v\r\n", app.ServicePort))
	b.WriteString(fmt.Sprintf("Service-Transport-Proto: %v\r\n", app.Protocol))
	b.WriteString(fmt.Sprintf("Service-Application-Proto: %v\r\n", GetApplicationProtocol1(app)))
	if app.HealthCheck != nil && len(app.HealthCheck.Protocol) != 0 {
		b.WriteString(fmt.Sprintf("Health-Check-Proto: %v\r\n", strings.ToLower(app.HealthCheck.Protocol)))
	}
	b.WriteString("\r\n")

	for _, task := range app.Backends {
		b.WriteString(fmt.Sprintf("%v:%v\n", task.Host, task.Port))
	}

	return ioutil.WriteFile(filename, b.Bytes(), 0660)
}

func (upstream *FilesManager) collectFiles() ([]string, error) {
	fileInfos, err := ioutil.ReadDir(upstream.BasePath)
	if err != nil {
		upstream.Log(fmt.Sprintf("Error reading directory %v. %v", upstream.BasePath, err))
		return nil, err
	}

	var fileNames []string
	for _, fileInfo := range fileInfos {
		fileNames = append(fileNames, filepath.Join(upstream.BasePath, fileInfo.Name()))
	}

	return fileNames, nil
}
