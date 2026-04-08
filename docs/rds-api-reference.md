# Seer RDS HTTP API Reference

Reference for the Seer RDS Core HTTP API endpoints used by Shingo. Extracted from the RDS Core HTTP API PDF and the shingo-core/rds client implementation.

## Base URL

All endpoints are relative to the RDS Core base URL (e.g. http://192.168.1.100:8080).

## Response Envelope

Most endpoints return a JSON envelope:

```json
{ "code": 0, "msg": "ok", "create_on": "2024-01-01T00:00:00Z" }
```

- code == 0 ? success
- code != 0 ? error; msg contains the error description

Some endpoints (order detail lookups) return order fields at the top level alongside the envelope fields, rather than nested under data.

---

## Orders

### Create Order (Block-based)

POST /setOrder

Creates a multi-block order with explicit location, operation, and bin task per block.

**Request:**

```json
{
  "id": "order-001",
  "externalId": "ext-001",         // optional
  "vehicle": "robot-1",            // optional, pin to specific robot
  "group": "group-a",             // optional, robot dispatch group
  "label": "label-x",             // optional, dispatch filter label
  "keyRoute": ["P1", "P2"],       // optional, forced route
  "keyTask": "JackLoad",          // optional, forced task
  "priority": 5,                  // optional, default 0
  "complete": true,               // true = no more blocks will be added
  "blocks": [
    {
      "blockId": "order-001_load",
      "location": "STATION-A",
      "operation": "",              // optional
      "operation_args": {},         // optional
      "binTask": "JackLoad",       // optional: JackLoad, JackUnload
      "goodsId": "goods-001",      // optional
      "scriptName": "",            // optional
      "scriptArgs": {},             // optional
      "postAction": {               // optional
        "configId": "action-1"
      }
    },
    {
      "blockId": "order-001_unload",
      "location": "STATION-B",
      "binTask": "JackUnload",
      "goodsId": "goods-001"
    }
  ]
}
```

**Response:** Standard envelope.

### Create Join Order (Pickup + Delivery)

POST /setOrder

Same endpoint, different request shape. A join order combines pickup and delivery in a single flat request.

**Request:**

```json
{
  "id": "join-001",
  "externalId": "ext-002",         // optional
  "fromLoc": "STATION-A",
  "toLoc": "STATION-B",
  "vehicle": "robot-1",            // optional
  "group": "group-a",             // optional
  "goodsId": "goods-001",          // optional
  "priority": 5,                  // optional
  "loadPostAction": { "configId": "action-1" },    // optional
  "unloadPostAction": { "configId": "action-2" }   // optional
}
```

**Response:** Standard envelope.

### Terminate Order

POST /terminate

Terminates one or more orders. Supports termination by order ID, order ID list, or vehicle.

**Request:**

```json
{
  "id": "order-001",               // optional, single order
  "idList": ["order-1", "order-2"], // optional, multiple orders
  "vehicles": ["robot-1"],         // optional, terminate all orders for vehicles
  "disableVehicle": false,
  "clearAll": true                // optional, terminate all orders
}
```

**Response:** Standard envelope.

### Get Order Details

GET /orderDetails/{id}

Returns full order detail. Note: RDS returns order fields at the top level (not nested under data).

**Response:**

```json
{
  "code": 0, "msg": "ok", "create_on": "...",
  "id": "order-001",
  "externalId": "ext-001",
  "vehicle": "robot-1",
  "group": "group-a",
  "state": "RUNNING",
  "complete": true,
  "priority": 5,
  "createTime": 1700000000000,
  "terminalTime": 0,
  "blocks": [
    {
      "blockId": "order-001_load",
      "location": "STATION-A",
      "state": "FINISHED",
      "containerName": "container-1",
      "goodsId": "goods-001",
      "operation": "",
      "binTask": "JackLoad",
      "operation_args": {},
      "script_args": {},
      "script_name": ""
    }
  ],
  "errors": [], "warnings": [], "notices": []
}
```

### Get Order by External ID

GET /orderDetailsByExternalId/{externalId}

Same response shape as Get Order Details.

### Get Order by Block ID

GET /orderDetailsByBlockId/{blockId}

Returns the parent order containing the specified block. Same response shape as Get Order Details.

### Get Block Details

GET /blockDetailsById/{blockId}

Returns details for a single block within an order.

**Response:**

```json
{
  "code": 0, "msg": "ok",
  "blockId": "order-001_load",
  "location": "STATION-A",
  "state": "FINISHED",
  "containerName": "container-1",
  "goodsId": "goods-001",
  "operation": "",
  "binTask": "JackLoad",
  "operation_args": {},
  "script_args": {},
  "script_name": ""
}
```

### List Orders

GET /orders?page={page}&size={size}

Returns a paginated list of orders.

**Response:**

```json
{
  "code": 0, "msg": "ok",
  "data": [ { ...orderDetail }, ... ]
}
```

### Set Priority

POST /setPriority

Changes the priority of a pending order.

**Request:** { "id": "order-001", "priority": 10 }

### Set Label

POST /setLabel

Sets a dispatch-filtering label on an order.

**Request:** { "id": "order-001", "label": "urgent" }

### Add Blocks (Incremental Orders)

POST /addBlocks

Appends blocks to an existing incremental order (one created with complete: false).

**Request:**

```json
{
  "id": "staged-001",
  "blocks": [ { "blockId": "b3", "location": "LOC-C", "binTask": "JackLoad" } ],
  "complete": true,
  "vehicle": "robot-2"            // optional, pin to specific robot
}
```

### Mark Complete

POST /markComplete

Marks an incremental order as complete — no more blocks can be added.

**Request:** { "id": "staged-001" }

### Order States

| State | Description | Terminal |
|-------|-------------|----------|
| CREATED | Order created, not yet dispatched | No |
| TOBEDISPATCHED | Queued for dispatch | No |
| RUNNING | Robot is executing | No |
| WAITING | Waiting for next block (incremental orders) | No |
| FINISHED | Successfully completed | Yes |
| FAILED | Execution failed | Yes |
| STOPPED | Manually stopped | Yes |

### Order Messages

RDS returns structured messages (errors/warnings/notices) on orders:

```json
{ "code": 5001, "desc": "Navigation blocked", "times": 3, "timestamp": 1700000000000 }
```

---

## Robots

### Get Robots Status

GET /robotsStatus

Returns status for all connected robots.

**Response:**

```json
{
  "code": 0, "msg": "ok",
  "report": [
    {
      "uuid": "abc-123",
      "vehicle_id": "robot-1",
      "connection_status": 1,        // 0=disconnected, 1=connected
      "dispatchable": true,
      "is_error": false,
      "procBusiness": true,          // robot has active task
      "network_delay": 12,
      "basic_info": {
        "ip": "192.168.1.50",
        "model": "P30",
        "version": "3.2.1",
        "current_area": ["Area-A"],
        "current_group": "default",
        "current_map": "floor1"
      },
      "rbk_report": {
        "x": 10.5, "y": 20.3, "angle": 90.0,
        "battery_level": 85.0,
        "charging": false,
        "current_station": "P1",
        "last_station": "P2",
        "task_status": 1,
        "blocked": false,
        "emergency": false,
        "reloc_status": 0,
        "containers": [
          { "container_name": "c1", "goods_id": "goods-1", "has_goods": true, "desc": "" }
        ],
        "available_containers": 0,
        "total_containers": 1
      },
      "current_order": null
    }
  ]
}
```

### Set Dispatchable

POST /dispatchable

Controls whether robots can receive new orders.

**Request:**

```json
{ "vehicles": ["robot-1"], "type": "dispatchable" }
```

	ype values: "dispatchable", "undispatchable_unignore", "undispatchable_ignore"

### Redo Failed Order

POST /redoFailedOrder

Retries the current failed block for specified robots.

**Request:** { "vehicles": ["robot-1"] }

### Manual Finish

POST /manualFinished

Marks the current block as manually finished.

**Request:** { "vehicles": ["robot-1"] }

### Lock (Preempt Control)

POST /lock

Takes exclusive manual control of robots (prevents RDS dispatch).

**Request:** { "vehicles": ["robot-1"] }

### Unlock (Release Control)

POST /unlock

Releases manual control.

**Request:** { "vehicles": ["robot-1"] }

### Pause Navigation

POST /gotoSitePause

Pauses robots in place.

**Request:** { "vehicles": ["robot-1"] }

### Resume Navigation

POST /gotoSiteResume

Resumes paused robots.

**Request:** { "vehicles": ["robot-1"] }

### Confirm Relocalization

POST /reLocConfirm

Confirms robot position after manual repositioning.

**Request:** { "vehicles": ["robot-1"] }

### Switch Map

POST /switchMap

Switches a robot to a different map.

**Request:** { "vehicle": "robot-1", "map": "floor2" }

### Get Robot Map

GET /robotSmap?vehicle={vehicle}&map={mapName}

Downloads a map file from a robot. Returns raw binary.

### Set Parameters (Temporary)

POST /setParams

Temporarily modifies robot parameters (lost on restart).

**Request:**

```json
{
  "vehicle": "robot-1",
  "body": { "pluginName": { "paramName": value } }
}
```

### Save Parameters (Permanent)

POST /saveParams

Permanently modifies robot parameters (survives restart). Same request shape as setParams.

### Restore Parameter Defaults

POST /reloadParams

Resets specific plugin parameters to factory defaults.

**Request:**

```json
{
  "vehicle": "robot-1",
  "body": [ { "plugin": "pluginName", "params": ["param1", "param2"] } ]
}
```

---

## Bins / Locations

### Get Bin Details

GET /binDetails

GET /binDetails?binGroups={group1,group2}

Returns bin fill status, optionally filtered by group.

**Response:**

```json
{
  "code": 0, "msg": "ok",
  "data": [
    { "id": "BIN-A1", "filled": true, "holder": 1, "status": 1 }
  ]
}
```

### Check Bins

POST /binCheck

Validates that bin locations exist and are valid.

**Request:** { "bins": ["BIN-A1", "BIN-A2"] }

**Response:**

```json
{
  "code": 0, "msg": "ok",
  "bins": [
    { "id": "BIN-A1", "exist": true, "valid": true, "status": { "point_name": "P1" } }
  ]
}
```

---

## Scene

### Get Scene

GET /scene

Returns the full RDS scene configuration (areas, points, bins, robot groups, etc.).

**Response:**

```json
{
  "code": 0, "msg": "ok",
  "scene": {
    "areas": [
      {
        "name": "Area-A",
        "logicalMap": {
          "advancedPoints": [
            {
              "className": "GeneralLocation",
              "instanceName": "P1",
              "desc": "",
              "dir": 0.0,
              "ignoreDir": false,
              "pos": { "x": 1.0, "y": 2.0, "z": 0.0 },
              "property": [ { "key": "label", "type": "string", "stringValue": "Station A" } ]
            }
          ],
          "binLocationsList": [
            {
              "binLocationList": [
                {
                  "className": "BinLocation",
                  "instanceName": "BIN-1",
                  "pointName": "P1",
                  "groupName": "rack-a",
                  "pos": { "x": 1.0, "y": 2.0, "z": 0.0 },
                  "property": []
                }
              ]
            }
          ]
        },
        "maps": [ { "mapName": "floor1", "md5": "...", "robotId": "" } ]
      }
    ],
    "robotGroup": [
      { "name": "default", "robot": [ { "id": "robot-1", "property": [] } ] }
    ],
    "blockGroup": [], "doors": [], "labels": [], "lifts": [],
    "binAreas": [], "binMonitors": [], "terminals": []
  }
}
```

### Download Scene

GET /downloadScene

Downloads the complete scene as raw binary.

### Upload Scene

POST /uploadScene

Uploads a new scene configuration. Content-Type: pplication/octet-stream.

### Sync Scene

POST /syncScene

Pushes the current scene to all connected robots. No request body.

---

## Containers / Goods

### Bind Container Goods

POST /setContainerGoods

Associates a goods ID with a container on a robot.

**Request:** { "vehicle": "robot-1", "containerName": "c1", "goodsId": "goods-1" }

### Unbind Goods

POST /clearGoods

Removes a goods binding by vehicle and goods ID.

**Request:** { "vehicle": "robot-1", "goodsId": "goods-1" }

### Unbind Container

POST /clearContainer

Removes all goods from a specific container.

**Request:** { "vehicle": "robot-1", "containerName": "c1" }

### Clear All Container Goods

POST /clearAllContainersGoods

Removes all goods from all containers on a robot.

**Request:** { "vehicle": "robot-1" }

---

## Devices

### Get Devices Status

GET /devicesDetails

GET /devicesDetails?devices={name1,name2}

Returns status of doors, lifts, and terminals.

**Response:**

```json
{
  "code": 0, "msg": "ok",
  "doors": [ { "name": "D1", "state": 1, "disabled": false, "reasons": [] } ],
  "lifts": [ { "name": "L1", "state": 0, "disabled": false, "reasons": [] } ],
  "terminals": [ { "id": "T1", "state": 0 } ]
}
```

### Call Terminal

POST /callTerminal

Sends a command to an external terminal device.

**Request:** { "id": "T1", "type": "command" }

### Call Door

POST /callDoor

Sends open/close commands to automated doors.

**Request:** [ { "name": "D1", "state": 1 } ] (array of door commands)

### Disable Door

POST /disableDoor

Enables or disables automatic door control.

**Request:** { "names": ["D1"], "disabled": true }

### Call Lift

POST /callLift

Sends commands to lifts/elevators.

**Request:** [ { "name": "L1", "target_area": "Area-B" } ] (array)

### Disable Lift

POST /disableLift

Enables or disables automatic lift control.

**Request:** { "names": ["L1"], "disabled": true }

---

## Mutex Groups

### Occupy Mutex Group

POST /getBlockGroup

Claims exclusive access to mutex groups for an order.

**Request:** { "id": "order-001", "blockGroup": ["zone-a"] }

**Response:** Bare JSON array (no envelope):

```json
[ { "name": "zone-a", "isOccupied": true, "occupier": "order-001" } ]
```

### Release Mutex Group

POST /releaseBlockGroup

Releases previously claimed mutex groups.

**Request:** { "id": "order-001", "blockGroup": ["zone-a"] }

**Response:** Bare JSON array (same shape as occupy).

### Get Mutex Group Status

GET /blockGroupStatus

Returns current status of all mutex groups.

**Response:** Bare JSON array:

```json
[ { "name": "zone-a", "isOccupied": true, "occupier": "order-001" } ]
```

---

## Simulation

### Get Sim Robot State Template

GET /getSimRobotStateTemplate

Returns the template for simulated robot state fields.

**Response:** { "code": 0, "msg": "ok", "data": { ... } }

### Update Sim Robot State

POST /updateSimRobotState

Sets the state of a simulated robot.

**Request:** Arbitrary JSON map of state fields.

---

## System

### Ping

GET /ping

Connectivity check. Returns product/version info.

**Response:** { "product": "RDS", "version": "3.2.1" }

Note: Does not use the standard envelope.

### Get Profiles

POST /getProfiles

Retrieves an RDS configuration file as raw JSON.

**Request:** { "file": "filename" }

### Get License Info

GET /licInfo

Returns current license information.

**Response:**

```json
{
  "code": 0, "msg": "ok",
  "data": {
    "maxRobots": 10,
    "expiry": "2025-12-31",
    "features": [ { "name": "simulation", "enabled": true } ]
  }
}
```

---

## ShinGo Integration Notes

### How Shingo uses RDS

Shingo integrates with RDS through the shingo-core/rds client package and the shingo-core/fleet/seerrds adapter:

- **ds.Client** — Low-level HTTP client (GET/POST helpers, response decoding, debug logging)
- **seerrds.Adapter** — Implements Shingo's leet.TrackingBackend, leet.RobotLister, leet.NodeOccupancyProvider, and leet.VendorProxy interfaces
- **ds.Poller** — Periodically polls active orders for state transitions and emits events through the engine pipeline

### Key patterns

- All transport orders use block-based orders with JackLoad/JackUnload bin tasks
- Incremental (staged) orders are created with complete: false, then blocks are appended via /addBlocks
- The adapter pins the vehicle during ReleaseOrder to prevent RDS from re-dispatching to a different robot
- State mapping from RDS to ShinGo dispatch status is in seerrds.MapState()
- Container goods bindings track which robot is carrying what
- The RDS explorer UI (/handlers_rds_explorer.go) allows sending arbitrary requests to the fleet backend

### Source PDFs

- RDSCore _HTTP API_AIVISON .pdf — Full RDS Core HTTP API documentation (in repo root)
- RDS UserManual-EN_AIVISON .pdf — RDS user manual (in repo root)
