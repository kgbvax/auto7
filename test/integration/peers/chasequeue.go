// SPDX-License-Identifier: AGPL-3.0-or-later

package peers

import (
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
)

// ChaseQueueSlot is the slot name of chase-queue.
const ChaseQueueSlot = "chase"

// ChaseQueue is a fake chase-queue at <site>/<station>/chase. A test
// publishes the whole /state document with Publish, sets /status with
// SetStatus and reads the recorded /cmd messages with Commands. It can
// publish on change only or with a heartbeat (StartHeartbeat).
type ChaseQueue struct {
	*Slot
}

// NewChaseQueue creates the fake and subscribes to its /cmd topic.
func NewChaseQueue(c pahomqtt.Client, site, station string, now func() time.Time) (*ChaseQueue, error) {
	s, err := NewSlot(c, site, station, ChaseQueueSlot, now)
	if err != nil {
		return nil, err
	}
	return &ChaseQueue{Slot: s}, nil
}
