// +build android

/*
 * Copyright (C) 2018 The "MysteriumNetwork/node" Authors.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package cmd

import (
	"github.com/mysteriumnetwork/node/core/node"
	"github.com/mysteriumnetwork/node/ui/noop"
)

// bootstrapServices loads all the components required for running services
func (di *Dependencies) bootstrapServices(nodeOptions node.Options) error {
	// Running services on mobile is not supported, nothing to bootstrap.
	return nil
}

func (di *Dependencies) registerConnections(nodeOptions node.Options) {
	di.registerNoopConnection()
}

func (di *Dependencies) bootstrapUIServer(options node.Options) {
	di.UIServer = noop.NewServer()
}

func (di *Dependencies) bootstrapMMN(options node.Options) {
	client := mmn.NewClient(options.BindAddress, options.MMN.Address)

	err := di.EventBus.SubscribeAsync(
		identity.IdentityUnlockTopic,
		mmn.OnIdentityUnlockCallback(client, di.IPResolver),
	)

	if err != nil {
		log.Error("Failed to get register to mmn event", err.Error())
	}
}
