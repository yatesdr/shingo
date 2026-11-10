# Seer RDS HTTP API Reference

Verified against: fe06474a

Reference for the Seer RDS Core HTTP API endpoints used by Shingo. Extracted from the RDS Core HTTP API PDF and the shingo-core/rds client implementation.

## Endpoint index

<!-- generated: shingo-core/rds endpoints -->

52 endpoints through 53 `rds` client funcs; the fleet adapter reaches 26 of the funcs (26 endpoints). **Adapter** is `yes` when `shingo-core/fleet/seerrds` calls the func, directly or through another `rds` func (the poller included).

| Endpoint | Method | `rds` func | Adapter |
|---|---|---|---|
| `/addBlocks` | POST | `Client.AddBlocks` | yes |
| `/binCheck` | POST | `Client.CheckBins` | yes |
| `/binDetails` | GET | `Client.GetBinDetails` | yes |
| `/blockDetailsById` | GET | `Client.GetBlockDetails` | no |
| `/blockGroupStatus` | GET | `Client.GetMutexGroupStatus` | no |
| `/callDoor` | POST | `Client.CallDoor` | no |
| `/callLift` | POST | `Client.CallLift` | no |
| `/callTerminal` | POST | `Client.CallTerminal` | no |
| `/clearAllContainersGoods` | POST | `Client.ClearAllContainerGoods` | yes |
| `/clearContainer` | POST | `Client.UnbindContainerGoods` | yes |
| `/clearGoods` | POST | `Client.UnbindGoods` | yes |
| `/devicesDetails` | GET | `Client.GetDevicesStatus` | no |
| `/disableDoor` | POST | `Client.DisableDoor` | no |
| `/disableLift` | POST | `Client.DisableLift` | no |
| `/dispatchable` | POST | `Client.SetDispatchable` | yes |
| `/downloadScene` | GET | `Client.DownloadScene` | no |
| `/fireOperations` | POST | `Client.SetFireAlarm` | yes |
| `/generalRobokitAPI` | POST | `Client.GeneralRobokitAPI` | yes |
| `/getBlockGroup` | POST | `Client.OccupyMutexGroup` | no |
| `/getProfiles` | POST | `Client.GetProfiles` | no |
| `/getSimRobotStateTemplate` | GET | `Client.GetSimStateTemplate` | no |
| `/gotoSitePause` | POST | `Client.PauseNavigation` | yes |
| `/gotoSiteResume` | POST | `Client.ResumeNavigation` | yes |
| `/isFire` | GET | `Client.GetFireAlarmStatus` | yes |
| `/licInfo` | GET | `Client.GetLicenseInfo` | no |
| `/lock` | POST | `Client.PreemptControl` | yes |
| `/manualFinished` | POST | `Client.ManualFinish` | yes |
| `/markComplete` | POST | `Client.MarkComplete` | no |
| `/orderDetails` | GET | `Client.GetOrderDetails` | yes |
| `/orderDetailsByBlockId` | GET | `Client.GetOrderByBlockID` | no |
| `/orderDetailsByExternalId` | GET | `Client.GetOrderByExternalID` | no |
| `/orders` | GET | `Client.ListOrders` | no |
| `/ping` | GET | `Client.Ping` | yes |
| `/redoFailedOrder` | POST | `Client.RedoFailed` | yes |
| `/releaseBlockGroup` | POST | `Client.ReleaseMutexGroup` | no |
| `/reloadParams` | POST | `Client.RestoreParamDefaults` | no |
| `/reLocConfirm` | POST | `Client.ConfirmRelocalization` | yes |
| `/robotSmap` | GET | `Client.GetRobotMap` | no |
| `/robotsStatus` | GET | `Client.GetRobotsStatusFull` | yes |
| `/saveParams` | POST | `Client.SetParamsPerm` | no |
| `/scene` | GET | `Client.GetScene` | yes |
| `/setContainerGoods` | POST | `Client.BindContainerGoods` | yes |
| `/setLabel` | POST | `Client.SetLabel` | no |
| `/setOrder` | POST | `Client.CreateJoinOrder` | no |
| `/setOrder` | POST | `Client.CreateOrder` | yes |
| `/setParams` | POST | `Client.SetParamsTemp` | no |
| `/setPriority` | POST | `Client.SetPriority` | yes |
| `/switchMap` | POST | `Client.SwitchMap` | yes |
| `/syncScene` | POST | `Client.SyncScene` | no |
| `/terminate` | POST | `Client.TerminateOrder` | yes |
| `/unlock` | POST | `Client.ReleaseControl` | yes |
| `/updateSimRobotState` | POST | `Client.UpdateSimState` | no |
| `/uploadScene` | POST | `Client.UploadScene` | no |

<!-- /generated -->

## Base URL

All endpoints are relative to the RDS Core base URL (e.g. http://192.0.2.10:8080).

## Response Envelope

Most endpoints return a JSON envelope:

```json
{ "code": 0, "msg": "ok", "create_on": "2024-01-01T00:00:00Z" }
```

- code 0: success
- code non-zero: error; msg contains the error description

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
  "keyRoute": ["P1", "P2"],       // optional, robot-selection assist (NOT a route constraint; see below)
  "keyTask": "load",              // optional, robot-selection assist: "load" or "unload" (see below)
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

**Robot-selection hints (`keyRoute`, `keyTask`):** These are optional fields that
help RDS pick which robot to assign. Shingo sets them per order when the
originating claim configures them: the Edge's `style_node_claims.key_route` / `.key_task`
are carried to Core's `orders.key_route` / `.key_task` at intake and onto the fleet
request at dispatch. Blank on a claim — the default — means "RDS auto-assigns".

Route points are validated at save time against the plant's SCENE, not against
Shingo's node list: Shingo works in APs, so its nodes are a subset of the map,
and a plain waypoint is a key route's primary use.

- `keyRoute` — extra via-waypoints a specific job should pass through on the way
  to its action points. Use only real map locations. `SELF_POSITION` is never
  allowed, and any point that does not exist or is unreachable kills the waybill
  at issue (RDS terminates it immediately).
- `keyTask` — `"load"` or `"unload"`.

**No-action waypoint block:** To force a stop at a location with no action, send
a block with only `blockId` and `location` (leave `binTask` and `operation`
blank). The robot stops there but runs no action or script.

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
  "createTime": 1700000000,
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

Returns status for all connected robots: the envelope plus a `report` array, one object per robot.

**Response:** the real key set is in two anonymised captures, one per plant, in
`shingo-core/fleet/seerrds/testdata`. `TestWireKeysAreCarriedOrExplained` (`wiredrift_test.go`) reads them and
fails on any wire key that is neither a field of `rds.RobotStatus` (or its nested structs) nor named, with a
reason, in the test's not-carried list; `mapRobotStatus` turns the struct into `fleet.RobotStatus`.

### Set Dispatchable

POST /dispatchable

Controls whether robots can receive new orders.

**Request:**

```json
{ "vehicles": ["robot-1"], "type": "dispatchable" }
```

`type` values: "dispatchable", "undispatchable_unignore", "undispatchable_ignore"

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

The fleet adapter does not use it: it downloads a robot's map through the Robokit relay below
(`Client.DownloadMap`).

### Robokit Relay

POST /generalRobokitAPI

Relays one Robokit call to a named robot on a named Robokit port, over RDS's own connection to that robot. The
plant network blocks every Robokit port but status from the Core box, so the relay is how Core reaches a robot's
onboard map; the laser frame goes through it too, so Core never opens a connection to a robot.

**Request:**

```json
{ "vehicle": "robot-1", "port": 19207, "code": 4011, "cmd": { "map_name": "floor1" } }
```

- `port` — the Robokit port: 19204 status, 19205 control, 19206 navigation, 19207 config.
- `code` — the Robokit API number. The adapter sends 4011 (download map, config port) and 1009 (laser frame,
  status port); `Client.GetRobotMapList` sends 1300 (which map a robot runs, and its hash).
- `cmd` — the Robokit call's own body; omitted when it has none.

**Response:** the robot's raw Robokit response. A failed relay still answers HTTP 200, with a non-zero `code`
(or `ret_code`) and a `msg` in the body; a relay that did not reach the robot can answer 200 with a bare
`{"code":0,"msg":"ok"}`. `Client.GeneralRobokitAPI` treats both as errors.

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

Uploads a new scene configuration. Content-Type: application/octet-stream.

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

## Fire Alarm

### Get Fire Alarm Status

GET /isFire

Returns whether the fleet's fire alarm is raised.

**Response:**

```json
{ "code": 0, "msg": "ok", "create_on": "2024-01-01T00:00:00Z", "is_fire": false }
```

`create_on` is when the alarm state last changed; RDS returns the epoch (`1970-01-01T00:00:00Z`) or nothing for
an alarm never raised, which `Client.GetFireAlarmStatus` reports as no time.

### Set Fire Alarm

POST /fireOperations

Raises or clears the fire alarm.

**Request:**

```json
{ "on": true, "autoResume": true }
```

- `on` — true raises the alarm, false clears it.
- `autoResume` — optional; sent only when true.

The fleet adapter exposes both as `fleet.FireAlarmController`.

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

- **rds.Client** — Low-level HTTP client (GET/POST helpers, response decoding, debug logging)
- **seerrds.Adapter** — Implements Shingo's fleet.TrackingBackend and the optional fleet interfaces RDS can serve: fleet.RobotLister, fleet.NodeOccupancyProvider, fleet.SceneSyncer, fleet.VendorProxy, fleet.FireAlarmController, fleet.RobotMapDownloader and fleet.LidarFrameReader among them
- **rds.Poller** — Periodically polls active orders for state transitions and emits events through the engine pipeline

### Key patterns

- All transport orders use block-based orders with JackLoad/JackUnload bin tasks
- Incremental (staged) orders are created with complete: false, then blocks are appended via /addBlocks
- The adapter pins the vehicle during ReleaseOrder to prevent RDS from re-dispatching to a different robot
- State mapping from RDS to ShinGo dispatch status is in seerrds.MapState()
- Container goods bindings track which robot is carrying what
- Robot maps and laser frames come through the Robokit relay (/generalRobokitAPI), on change and hourly respectively, never on the status poll
- The RDS explorer page (shingo-core/www/handlers_rds_explorer.go) allows sending arbitrary requests to the fleet backend

### Source PDFs

This reference was derived from two vendor PDFs. **Neither is in the repo** — vendor PDFs are kept
out of version control, and `git ls-files '*.pdf'` lists nothing. Ask for them rather than looking
for them in a checkout.

- "RDSCore _HTTP API_AIVISON .pdf" — full RDS Core HTTP API documentation
- "RDS UserManual-EN_AIVISON .pdf" — RDS user manual
