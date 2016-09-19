package main

// EventListener provides an interface for hooking into
// standard service discovery API calls, such as adding and removing
// backends from load balancers (app clusters).
type EventListener interface {
	// Startup is invoked upon application startup
	Startup()

	// Shutdown is invoked upon application shutdown
	Shutdown()

	// Apply installs the load balancer for all apps.
	// This function is invoked upon startup to synchronize with the current
	// state.
	Apply(apps []*AppCluster)

	// AddTask must add the given backend to the cluster.
	AddTask(task *AppBackend, app *AppCluster)

	// RemoveTask must remove the given backend from the cluster.
	RemoveTask(task *AppBackend, app *AppCluster)
}
