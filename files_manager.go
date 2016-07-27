package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"

	"github.com/dawanda/go-mesos/marathon"
)

type FilesManager struct {
	Enabled  bool
	Verbose  bool
	BasePath string
}

func (upstream *FilesManager) Log(msg string) {
	if upstream.Verbose {
		log.Printf("upstream: %v\n", msg)
	}
}

func (manager *FilesManager) Setup() error {
	return nil
}

func (manager *FilesManager) IsEnabled() bool {
	return manager.Enabled
}

func (manager *FilesManager) SetEnabled(value bool) {
	if value != manager.Enabled {
		manager.Enabled = value
	}
}

func (upstream *FilesManager) Remove(appID string, taskID string, app *marathon.App) error {
	if app != nil {
		_, err := upstream.writeApp(app)
		return err
	} else {
		// TODO: remove files for app-$portIndex
		return nil
	}
}

func (upstream *FilesManager) Update(app *marathon.App, taskID string) error {
	_, err := upstream.writeApp(app)
	return err
}

func (upstream *FilesManager) Apply(apps []*marathon.App, force bool) error {
	err := os.MkdirAll(upstream.BasePath, 0770)
	if err != nil {
		return err
	}

	var newFiles []string
	oldFiles, err := upstream.collectFiles()
	if err != nil {
		return err
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

	return nil
}

func (upstream *FilesManager) writeApp(app *marathon.App) ([]string, error) {
	var files []string

	for portIndex, port := range app.Ports {
		app_id := PrettifyAppId(app.Id, portIndex, port)
		cfgfile := filepath.Join(upstream.BasePath, app_id+".instances")
		tmpfile := cfgfile + ".tmp"

		err := upstream.writeFile(tmpfile, app_id, portIndex, app)
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
	}
	return files, nil
}

func (upstream *FilesManager) writeFile(filename string, appId string,
	portIndex int, app *marathon.App) error {

	var b bytes.Buffer
	b.WriteString(fmt.Sprintf("Service-Name: %v\r\n", appId))
	b.WriteString(fmt.Sprintf("Service-Port: %v\r\n", app.Ports[portIndex]))
	b.WriteString(fmt.Sprintf("Service-Transport-Proto: %v\r\n", GetTransportProtocol(app, portIndex)))
	b.WriteString(fmt.Sprintf("Service-Application-Proto: %v\r\n", GetApplicationProtocol(app, portIndex)))
	if proto := GetHealthCheckProtocol(app, portIndex); len(proto) != 0 {
		b.WriteString(fmt.Sprintf("Health-Check-Proto: %v\r\n", GetApplicationProtocol(app, portIndex)))
	}
	b.WriteString("\r\n")

	for _, task := range app.Tasks {
		b.WriteString(fmt.Sprintf("%v:%v\n", task.Host, task.Ports[portIndex]))
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
