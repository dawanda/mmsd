package main

type EventListener interface {
	Startup()
	Shutdown()

	Apply(apps []AppCluster)
	AddTask(task AppBackend, app AppCluster)
	RemoveTask(task AppBackend, app AppCluster)
}
