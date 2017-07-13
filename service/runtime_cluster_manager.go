//
// DISCLAIMER
//
// Copyright 2017 ArangoDB GmbH, Cologne, Germany
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Copyright holder is ArangoDB GmbH, Cologne, Germany
//
// Author Ewout Prangsma
//

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/arangodb-helper/arangodb/service/agency"
	logging "github.com/op/go-logging"
)

const (
	masterURLTTL = time.Second * 30
)

var (
	masterURLKey = []string{"arangodb-helper", "arangodb", "master"}
)

// runtimeClusterManager keeps the cluster configuration up to date during a running state.
type runtimeClusterManager struct {
	runtimeContext runtimeClusterManagerContext
}

// runtimeClusterManagerContext provides a context for the runtimeClusterManager.
type runtimeClusterManagerContext interface {
	// ClusterConfig returns the current cluster configuration and the current peer
	ClusterConfig() (ClusterConfig, *Peer, ServiceMode)

	// ChangeState alters the current state of the service
	ChangeState(newState State)

	// PrepareDatabaseServerRequestFunc returns a function that is used to
	// prepare a request to a database server (including authentication).
	PrepareDatabaseServerRequestFunc() func(*http.Request) error

	// UpdateClusterConfig updates the current cluster configuration.
	UpdateClusterConfig(ClusterConfig)
}

// Create a client for the agency
func (s *runtimeClusterManager) createAgencyAPI() (agency.API, error) {
	prepareReq := s.runtimeContext.PrepareDatabaseServerRequestFunc()
	// Get cluster config
	clusterConfig, _, _ := s.runtimeContext.ClusterConfig()
	// Create client
	return clusterConfig.CreateAgencyAPI(prepareReq)
}

// getMasterURL tries to get the URL of the current master from
// a well known location in the agency.
func (s *runtimeClusterManager) getMasterURL(ctx context.Context) (string, error) {
	// Get api client
	api, err := s.createAgencyAPI()
	if err != nil {
		return "", maskAny(err)
	}
	// Try to read master URL
	ctx, cancel := context.WithTimeout(ctx, time.Second*10)
	defer cancel()
	if result, err := api.ReadKey(ctx, masterURLKey); err != nil {
		if agency.IsKeyNotFound(err) {
			return "", nil
		}
		return "", maskAny(err)
	} else if strResult, ok := result.(string); ok {
		return strResult, nil
	} else {
		return "", maskAny(fmt.Errorf("Invalid value type at key: %v", reflect.TypeOf(result)))
	}
}

// tryBecomeMaster tries to write our URL into a well known location in the agency,
// assuming there is no master.
func (s *runtimeClusterManager) tryBecomeMaster(ctx context.Context, ownURL string) error {
	// Get api client
	api, err := s.createAgencyAPI()
	if err != nil {
		return maskAny(err)
	}
	// Try to write our master URL
	ctx, cancel := context.WithTimeout(ctx, time.Second*10)
	defer cancel()
	if err := api.WriteKeyIfEmpty(ctx, masterURLKey, ownURL, masterURLTTL); err != nil {
		return maskAny(err)
	}
	return nil
}

// tryRemainMaster tries to write our URL into a well known location in the agency,
// assuming we're already the master.
func (s *runtimeClusterManager) tryRemainMaster(ctx context.Context, ownURL string) error {
	// Get api client
	api, err := s.createAgencyAPI()
	if err != nil {
		return maskAny(err)
	}
	// Try to update our master URL
	ctx, cancel := context.WithTimeout(ctx, time.Second*10)
	defer cancel()
	if err := api.WriteKeyIfEqualTo(ctx, masterURLKey, ownURL, ownURL, masterURLTTL); err != nil {
		return maskAny(err)
	}
	return nil
}

// updateClusterConfiguration asks the master at given URL for the latest cluster configuration.
func (s *runtimeClusterManager) updateClusterConfiguration(ctx context.Context, masterURL string) error {
	var helloURL string
	if strings.HasSuffix(masterURL, "/") {
		helloURL = masterURL + "hello"
	} else {
		helloURL = masterURL + "/hello"
	}
	// Perform request
	r, err := httpClient.Get(helloURL)
	if err != nil {
		return maskAny(err)
	}
	// Parse result
	defer r.Body.Close()
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return maskAny(err)
	}
	var clusterConfig ClusterConfig
	if err := json.Unmarshal(body, &clusterConfig); err != nil {
		return maskAny(err)
	}
	// We've received a cluster config
	s.runtimeContext.UpdateClusterConfig(clusterConfig)

	return nil
}

// Run keeps the cluster configuration up to date, either as master or as slave
// during a running state.
func (s *runtimeClusterManager) Run(ctx context.Context, log *logging.Logger, runtimeContext runtimeClusterManagerContext) {
	s.runtimeContext = runtimeContext
	_, myPeer, mode := runtimeContext.ClusterConfig()
	if !mode.IsClusterMode() {
		// Cluster manager is only relevant in cluster mode
		return
	}
	if myPeer == nil {
		// We need to know our own peer
		log.Error("Cannot run runtime cluster manager without own peer")
		return
	}
	ownURL := myPeer.CreateStarterURL("/")

	for {
		var delay time.Duration
		// Loop until stopping
		if ctx.Err() != nil {
			// Stop requested
			return
		}

		// Try to get master URL
		masterURL, err := s.getMasterURL(ctx)
		if err != nil {
			// Cannot obtain master url, wait a while and try again
			log.Debugf("Failed to get master URL, retrying in 5sec (%#v)", err)
			delay = time.Second * 5
		} else if masterURL == "" {
			// There is currently no master, try to become master
			log.Debug("There is no current master, try to become master")
			if err := s.tryBecomeMaster(ctx, ownURL); err != nil {
				log.Debugf("tried to become master but failed: %#v", err)
				runtimeContext.ChangeState(stateRunningSlave)
			} else {
				log.Info("Just became master")
				runtimeContext.ChangeState(stateRunningMaster)
			}
			// Have update delay before trying to remain master
			delay = masterURLTTL / 3
		} else if masterURL == ownURL {
			// We are the master, update our entry in the agency
			log.Debug("We're master, try to remain it")
			runtimeContext.ChangeState(stateRunningMaster)

			// Update agency
			if err := s.tryRemainMaster(ctx, ownURL); err != nil {
				log.Info("Failed to remain master: %#v", err)
				runtimeContext.ChangeState(stateRunningSlave)

				// Retry soon
				delay = time.Second
			} else {
				// I'm still the master
				// wait a bit before updating master URL
				delay = masterURLTTL / 3
			}
		} else {
			// We are slave, try to update cluster configuration from master
			log.Debugf("We're slave, try to update cluster config from %s", masterURL)
			runtimeContext.ChangeState(stateRunningSlave)

			// Ask current master for cluster configuration
			if err := s.updateClusterConfiguration(ctx, masterURL); err != nil {
				log.Warningf("Failed to load cluster configuration from %s: %#v", masterURL, err)
			}

			// Wait a bit until re-updating the configuration
			delay = time.Second * 15
		}

		// Wait a bit
		select {
		case <-time.After(delay):
		// Delay over, just continue
		case <-ctx.Done():
			// We're asked to stop
			return
		}
	}
}
