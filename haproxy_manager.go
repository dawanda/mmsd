package main

import (
	"io/ioutil"
	"net"
	"os"

	"github.com/christianparpart/serviced/marathon"
)

type HaproxyMgr struct {
	Enabled        bool
	Verbose        bool
	ConfigPath     string
	ConfigTailPath string
	PidFile        string
	ManagementAddr net.IP
	ManagementPort uint
}

func (manager *HaproxyMgr) IsEnabled() bool {
	return manager.Enabled
}

func (manager *HaproxyMgr) SetEnabled(value bool) {
	if value != manager.Enabled {
		manager.Enabled = value
	}
}

func (manager *HaproxyMgr) Apply(apps []*marathon.App, force bool) error {
	var fullConfig string

	for _, app := range apps {
		config, err := manager.MakeConfig(app)
		if err != nil {
			return err
		}
		fullConfig += config
	}

	return manager.UpdateConfig(fullConfig)
}

func (manager *HaproxyMgr) MakeConfig(app *marathon.App) (string, error) {
	return "", nil
}

func (manager *HaproxyMgr) UpdateConfig(config string) error {
	if len(manager.ConfigTailPath) > 0 {
		tail, err := ioutil.ReadFile(manager.ConfigTailPath)
		if err != nil {
			return err
		}
		config += string(tail)
	}

	os.Rename(manager.ConfigPath, manager.ConfigPath+".old")

	err := ioutil.WriteFile(manager.ConfigPath, []byte(config), 0666)
	if err != nil {
		return err
	}

	return manager.ReloadConfig()
}

func (manager *HaproxyMgr) ReloadConfig() error {
	return nil // TODO
}

func (manager *HaproxyMgr) Update(app *marathon.App, task *marathon.Task) error {
	return nil
}
