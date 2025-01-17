// @copyright 2021-Present Couchbase, Inc.
//
// Use of this software is governed by the Business Source License included
// in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
// in that file, in accordance with the Business Source License, use of this
// software will be governed by the Apache License, Version 2.0, included in
// the file licenses/APL2.txt.
package indexer

import (
	"github.com/couchbase/cbauth/service"
	"github.com/couchbase/indexing/secondary/logging"
)

////////////////////////////////////////////////////////////////////////////////////////////////////
// MasterServiceManager class
////////////////////////////////////////////////////////////////////////////////////////////////////

// MasterServiceManager is used to work around cbauth's monolithic service manager architecture that
// requires a singleton to implement all the different interfaces, as cbauth requires this to be
// registered only once. These are thus "implemented" here as delegates to the real GSI implementing
// clasess.
//
// ns_server interfaces implemented (defined in cbauto/service/interface.go)
//   AutofailoverManager -- GSI class: AutofailoverServiceManager (autofailover_service_manager.go)
//   Manager             -- GSI class: RebalanceServiceManager (rebalance_service_manager.go)
type MasterServiceManager struct {
	autofail *AutofailoverServiceManager
	rebal    *RebalanceServiceManager
}

// NewMasterServiceManager is the constructor for the MasterServiceManager class
func NewMasterServiceManager(autofailoverMgr *AutofailoverServiceManager,
	rebalMgr *RebalanceServiceManager) *MasterServiceManager {
	this := &MasterServiceManager{
		autofail: autofailoverMgr,
		rebal:    rebalMgr,
	}
	go this.registerWithServer()
	return this
}

// registerWithServer runs in a goroutine that registers this object as the singleton handler
// implementing the ns_server RPC interfaces Manager (historically generic name for Rebalance
// manager) and AutofailoverManager. Errors are logged but indexer will continue on regardless.
func (this *MasterServiceManager) registerWithServer() {
	const method = "MasterServiceManager::registerWithServer:" // for logging

	// Ensure this class implements the interfaces we intend. The type assertions will panic if not.
	var iface interface{} = this
	logging.Infof("%v %T implements service.AutofailoverManager; %T implements service.Manager",
		method, iface.(service.AutofailoverManager), iface.(service.Manager))

	// Unless it returns an error, RegisterManager will actually run forever instead of returning
	err := service.RegisterManager(this, nil)
	if err != nil {
		logging.Errorf("%v Failed to register with Cluster Manager. err: %v", method, err)
		return
	}
	logging.Infof("%v Registered with Cluster Manager", method)
}

////////////////////////////////////////////////////////////////////////////////////////////////////
// ns_server AutofailoverManager interface methods
////////////////////////////////////////////////////////////////////////////////////////////////////

func (this *MasterServiceManager) HealthCheck() (*service.HealthInfo, error) {
	return this.autofail.HealthCheck()
}

func (this *MasterServiceManager) IsSafe(nodeUUIDs []service.NodeID) error {
	return this.autofail.IsSafe(nodeUUIDs)
}

////////////////////////////////////////////////////////////////////////////////////////////////////
// ns_server Manager interface methods (for Rebalance)
////////////////////////////////////////////////////////////////////////////////////////////////////

func (this *MasterServiceManager) GetNodeInfo() (*service.NodeInfo, error) {
	return this.rebal.GetNodeInfo()
}

func (this *MasterServiceManager) Shutdown() error {
	return this.rebal.Shutdown()
}

func (this *MasterServiceManager) GetTaskList(rev service.Revision, cancel service.Cancel) (
	*service.TaskList, error) {
	return this.rebal.GetTaskList(rev, cancel)
}

func (this *MasterServiceManager) CancelTask(id string, rev service.Revision) error {
	return this.rebal.CancelTask(id, rev)
}

func (this *MasterServiceManager) GetCurrentTopology(rev service.Revision, cancel service.Cancel) (*service.Topology, error) {
	return this.rebal.GetCurrentTopology(rev, cancel)
}

func (this *MasterServiceManager) PrepareTopologyChange(change service.TopologyChange) error {
	return this.rebal.PrepareTopologyChange(change)
}

func (this *MasterServiceManager) StartTopologyChange(change service.TopologyChange) error {
	return this.rebal.StartTopologyChange(change)
}
