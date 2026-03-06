// Template bodies for POST endpoints
var EP_JOIN_ORDER = JSON.stringify({id:"",externalId:"",fromLoc:"",toLoc:"",priority:0,vehicle:"",group:"",goodsId:""},null,2);
var EP_SET_ORDER = JSON.stringify({id:"",externalId:"",vehicle:"",group:"",label:"",priority:0,complete:true,blocks:[{blockId:"",location:"",operation:"",binTask:"",goodsId:""}]},null,2);
var EP_TERMINATE = JSON.stringify({id:"",idList:[],vehicles:[],disableVehicle:false,clearAll:false},null,2);
var EP_SET_PRIORITY = JSON.stringify({id:"",priority:0},null,2);
var EP_SET_LABEL = JSON.stringify({id:"",label:""},null,2);
var EP_MARK_COMPLETE = JSON.stringify({id:""},null,2);
var EP_ADD_BLOCKS = JSON.stringify({id:"",blocks:[{blockId:"",location:"",operation:"",binTask:""}],complete:false},null,2);
var EP_VEHICLES = JSON.stringify({vehicles:[""]},null,2);
var EP_DISPATCHABLE = JSON.stringify({vehicles:[""],type:"dispatchable"},null,2);
var EP_MODIFY_PARAMS = JSON.stringify({vehicle:"",body:{"pluginName":{"paramName":"value"}}},null,2);
var EP_RESTORE_PARAMS = JSON.stringify({vehicle:"",body:[{plugin:"",params:[""]}]},null,2);
var EP_TOGGLE_MAP = JSON.stringify({vehicle:"",map:""},null,2);
var EP_BIND_GOODS = JSON.stringify({vehicle:"",containerName:"",goodsId:""},null,2);
var EP_UNBIND_GOODS = JSON.stringify({vehicle:"",goodsId:""},null,2);
var EP_UNBIND_CONTAINER = JSON.stringify({vehicle:"",containerName:""},null,2);
var EP_CLEAR_ALL_GOODS = JSON.stringify({vehicle:""},null,2);
var EP_MUTEX = JSON.stringify({id:"",blockGroup:[""]},null,2);
var EP_BIN_CHECK = JSON.stringify({bins:[""]},null,2);
var EP_CALL_TERMINAL = JSON.stringify({id:"",type:"read"},null,2);
var EP_CALL_DOOR = JSON.stringify([{name:"",state:1}],null,2);
var EP_DISABLE_DEVICE = JSON.stringify({names:[""],disabled:true},null,2);
var EP_CALL_LIFT = JSON.stringify([{name:"",target_area:""}],null,2);
var EP_UPDATE_SIM = JSON.stringify({vehicle_id:"",battery_percentage:1.0},null,2);
var EP_GET_PROFILES = JSON.stringify({file:"properties.json"},null,2);

// Endpoint descriptions
var EP_INFO = {
  "/setOrder": {
    title: "Set Order (Block-Based)",
    desc: "Creates a transport order composed of one or more blocks (steps). The robot executes blocks in sequence. Use this for multi-stop routes or custom workflows. For simple A-to-B transport, use the join order variant instead." +
      "<br><br><strong style='font-size:0.95rem'>Block Types</strong><br>" +
      "There are three types of blocks, determined by which fields you set:" +
      "<br><br><em>1. Bin Location Block</em> &ndash; Robot goes to a named storage bin location (e.g. <code>Loc-01</code>) and performs the action configured for that bin. Set <code>location</code> + <code>binTask</code> (typically <code>\"Load\"</code> or <code>\"Unload\"</code>). Optionally set <code>goodsId</code> to track the cargo." +
      "<br><br><em>2. Map Point + Operation</em> &ndash; Robot goes to a map point (e.g. <code>AP8</code>) and runs a hardware mechanism action. Set <code>location</code> + <code>operation</code> (e.g. <code>\"JackLoad\"</code>). Available operations depend on the robot model config. Use <code>operationArgs</code> for parameters like <code>{\"recognize\": true}</code>." +
      "<br><br><em>3. Map Point + Script</em> &ndash; Robot goes to a map point and executes a Python script. Set <code>location</code> + <code>operation: \"Script\"</code> + <code>scriptName</code> (e.g. <code>\"myScript.py\"</code>). Pass arguments via <code>scriptArgs</code>." +
      "<br><br>Every block needs a unique <code>blockId</code>. Blocks execute in array order. If <code>complete: false</code>, you can append more blocks later with <code>addBlocks</code>.",
    params: "<strong>id</strong> &ndash; unique order ID (required)<br><strong>blocks[]</strong> &ndash; array of block objects (see block types above)<br><strong>complete</strong> &ndash; true = all blocks provided; false = more will be added via addBlocks<br><strong>vehicle</strong> &ndash; assign a specific robot (optional)<br><strong>group</strong> &ndash; restrict to a robot group (optional)<br><strong>priority</strong> &ndash; higher value = dispatched sooner (optional)<br><strong>externalId</strong> &ndash; your own reference ID (optional, non-unique)<br><strong>keyRoute</strong> &ndash; waypoint hints to assist dispatch (optional)",
    response: "Returns the created order with state, assigned vehicle, and block details. Check <code>code == 0</code> for success."
  },
  "/setOrder_join": {
    title: "Set Join Order (Point-to-Point)",
    desc: "Creates a simple pickup-and-delivery order via <code>POST /setOrder</code> with <code>fromLoc</code>/<code>toLoc</code> fields. The robot goes to <code>fromLoc</code>, loads, then goes to <code>toLoc</code> and unloads. This is the most common order type for material transport. Internally creates two sub-orders: <code>{id}_load</code> and <code>{id}_unload</code>.",
    params: "<strong>id</strong> &ndash; unique order ID<br><strong>fromLoc</strong> &ndash; pickup location name<br><strong>toLoc</strong> &ndash; delivery location name<br><strong>priority</strong> &ndash; higher value = dispatched sooner<br><strong>vehicle</strong> &ndash; assign a specific robot (optional)",
    response: "Returns the created order. Check <code>code == 0</code> for success."
  },
  "/markComplete": {
    title: "Mark Order Complete",
    desc: "Tells RDS that no more blocks will be added to an order that was created with <code>complete: false</code>. Once marked complete, the order will finish after its last block executes. Only needed for incrementally-built orders.",
    params: "<strong>id</strong> &ndash; the order ID to mark complete",
    response: "Success/failure status. <code>code == 0</code> means confirmed."
  },
  "/terminate": {
    title: "Terminate Order",
    desc: "Cancels one or more active orders. Can target orders by ID, by order list, or by robot. Use <code>disableVehicle: true</code> to also take the robot offline after cancellation. Use <code>clearAll: true</code> to cancel every active order in the system.",
    params: "<strong>id</strong> &ndash; single order ID<br><strong>idList</strong> &ndash; array of order IDs<br><strong>vehicles</strong> &ndash; cancel all orders on these robots<br><strong>disableVehicle</strong> &ndash; mark robots as non-dispatchable after cancel<br><strong>clearAll</strong> &ndash; cancel everything",
    response: "Success/failure status."
  },
  "/orderDetails": {
    title: "Query Order by ID",
    desc: "Retrieves the full details of an order by its RDS order ID. Append the order ID to the path: <code>/orderDetails/{id}</code>. Shows current state, assigned robot, block execution progress, and timing.",
    params: "<strong>id</strong> &ndash; the order ID (path parameter)",
    response: "Full order object with state, blocks, assigned vehicle, timestamps. States: CREATED, TOBEDISPATCHED, RUNNING, FINISHED, FAILED, STOPPED."
  },
  "/orderDetailsByExternalId": {
    title: "Query Order by External ID",
    desc: "Looks up an order using the external ID you provided when creating it. Append the external ID to the path: <code>/orderDetailsByExternalId/{id}</code>. Useful when your system tracks its own IDs separate from RDS.",
    params: "<strong>id</strong> &ndash; your external ID (path parameter)",
    response: "Full order details including state, blocks, and assigned vehicle."
  },
  "/orderDetailsByBlockId": {
    title: "Query Order by Block ID",
    desc: "Finds the parent order that contains a specific block. Append the block ID to the path: <code>/orderDetailsByBlockId/{id}</code>. Useful for tracing which order a particular sub-task belongs to.",
    params: "<strong>id</strong> &ndash; the block ID to search for (path parameter)",
    response: "Full order object containing the matching block."
  },
  "/orders": {
    title: "Paging Query Orders",
    desc: "Lists orders with pagination. Returns a page of orders sorted by creation time. Use for browsing order history or building dashboards.",
    params: "<strong>page</strong> &ndash; page number, starting at 0<br><strong>size</strong> &ndash; results per page (default 20)<br>Optional filters: <strong>state</strong>, <strong>vehicle</strong>, <strong>group</strong>",
    response: "Paged result with <code>totalElements</code>, <code>totalPages</code>, and array of order objects."
  },
  "/setPriority": {
    title: "Set Order Priority",
    desc: "Changes the dispatch priority of a pending order. Higher values are dispatched first. Only affects orders not yet assigned to a robot.",
    params: "<strong>id</strong> &ndash; order ID<br><strong>priority</strong> &ndash; new priority value (integer)",
    response: "Success/failure status."
  },
  "/setLabel": {
    title: "Set Order Label",
    desc: "Sets a robot-selection label on an order for dispatch filtering. Only robots whose label matches the order label will be considered for dispatch. This controls which robot group can fulfill the order, not just metadata.",
    params: "<strong>id</strong> &ndash; order ID<br><strong>label</strong> &ndash; dispatch label to match against robot labels",
    response: "Success/failure status."
  },
  "/addBlocks": {
    title: "Add Blocks to Order",
    desc: "Appends additional steps to an existing order that was created with <code>complete: false</code>. Allows building up a route incrementally. Set <code>complete: true</code> when adding the final batch. See <code>setOrder</code> info for full block type reference.",
    params: "<strong>id</strong> &ndash; order ID<br><strong>blocks[]</strong> &ndash; new block objects to append (same format as setOrder blocks)<br><strong>complete</strong> &ndash; true if these are the last blocks",
    response: "Updated order object with all blocks."
  },
  "/redoFailedOrder": {
    title: "Redo Failed Order",
    desc: "Retries the current failed block on one or more robots. The robot will re-attempt the operation that failed (e.g. a failed load or navigation). Use when the physical issue has been resolved.",
    params: "<strong>vehicles</strong> &ndash; array of robot names to retry",
    response: "Success/failure status for each robot."
  },
  "/manualFinished": {
    title: "Manual Finished",
    desc: "Manually marks the current block as finished for one or more robots, skipping whatever operation was in progress. Use when a block is stuck and you want to force the order forward (e.g. material was moved by hand).",
    params: "<strong>vehicles</strong> &ndash; array of robot names",
    response: "Success/failure status for each robot."
  },
  "/blockDetailsById": {
    title: "Query Block Details",
    desc: "Gets detailed information about a specific block (step) within an order. Append the block ID to the path: <code>/blockDetailsById/{id}</code>. Shows execution state, timing, and assigned robot.",
    params: "<strong>id</strong> &ndash; block ID (path parameter)",
    response: "Block object with state, location, operation, start/end times."
  },
  "/robotsStatus": {
    title: "Robot Status",
    desc: "Returns the live status of all robots in the fleet. Includes position, battery, current order, navigation state, and error info. This is the primary monitoring endpoint.",
    params: "None",
    response: "Array of robot objects with: name, IP, battery %, position (x/y/heading), current order ID, state (idle/running/charging/error), and dispatchable flag."
  },
  "/robotSmap": {
    title: "Fetch Robot Map",
    desc: "Downloads the map file currently loaded on a specific robot. Returns the raw map data (SMAP format). Useful for verifying which map version a robot is running.",
    params: "<strong>vehicle</strong> &ndash; robot/vehicle name (query parameter)<br><strong>map</strong> &ndash; map name (query parameter)",
    response: "Raw map data (binary/text SMAP format)."
  },
  "/lock": {
    title: "Preempt Control (Lock)",
    desc: "Takes exclusive manual control of one or more robots, pausing their current automated tasks. The robots will stop accepting dispatch orders until released via <code>/unlock</code>. Use for maintenance or manual driving.",
    params: "<strong>vehicles</strong> &ndash; array of robot names to lock",
    response: "Success/failure status."
  },
  "/unlock": {
    title: "Release Control (Unlock)",
    desc: "Releases one or more robots from manual control, returning them to the automated dispatch pool. Pass an empty array to release all locked robots.",
    params: "<strong>vehicles</strong> &ndash; array of robot names to unlock (empty = release all)",
    response: "Success/failure status."
  },
  "/setParams": {
    title: "Temporary Modify Parameters",
    desc: "Changes robot parameters (e.g. speed, acceleration) temporarily. Changes are lost when the robot restarts. The body is nested by plugin name, then parameter name/value.",
    params: "<strong>vehicle</strong> &ndash; robot name<br><strong>body</strong> &ndash; nested object: <code>{\"pluginName\": {\"paramName\": \"value\"}}</code>",
    response: "Success/failure status."
  },
  "/saveParams": {
    title: "Permanent Modify Parameters",
    desc: "Changes robot parameters permanently. Survives robot restart. Use with caution as incorrect values can affect robot safety and performance. Same body format as <code>/setParams</code>.",
    params: "<strong>vehicle</strong> &ndash; robot name<br><strong>body</strong> &ndash; nested object: <code>{\"pluginName\": {\"paramName\": \"value\"}}</code>",
    response: "Success/failure status."
  },
  "/reloadParams": {
    title: "Restore Parameter Defaults",
    desc: "Resets specific plugin parameters on a robot back to factory defaults. Takes a list of plugins and their parameter names to restore, not a blanket reset.",
    params: "<strong>vehicle</strong> &ndash; robot name<br><strong>body</strong> &ndash; array of <code>{\"plugin\": \"name\", \"params\": [\"param1\", \"param2\"]}</code>",
    response: "Success/failure status."
  },
  "/switchMap": {
    title: "Switch Robot Map",
    desc: "Switches a robot to a different map. The robot must be on the new map's floor/area for this to work. Used in multi-floor or multi-zone deployments.",
    params: "<strong>vehicle</strong> &ndash; robot name<br><strong>map</strong> &ndash; target map name",
    response: "Success/failure status."
  },
  "/dispatchable": {
    title: "Configure Dispatchable Status",
    desc: "Controls whether robots are available for order dispatch. Set to <code>dispatchable</code> to allow a robot to receive orders, <code>undispatchable_unignore</code> to take offline (robot finishes current order), or <code>undispatchable_ignore</code> to take offline immediately.",
    params: "<strong>vehicles</strong> &ndash; array of robot names<br><strong>type</strong> &ndash; \"dispatchable\", \"undispatchable_unignore\", or \"undispatchable_ignore\"",
    response: "Success/failure status."
  },
  "/reLocConfirm": {
    title: "Confirm Relocalization",
    desc: "Confirms that one or more robots have been correctly repositioned on the map after a manual move or localization drift. Call this after physically placing robots at known locations.",
    params: "<strong>vehicles</strong> &ndash; array of robot names",
    response: "Success/failure status."
  },
  "/gotoSitePause": {
    title: "Pause Navigation",
    desc: "Pauses one or more robots in place. The robots stop moving but retain their current orders. Call <code>/gotoSiteResume</code> to continue. Useful for temporary holds.",
    params: "<strong>vehicles</strong> &ndash; array of robot names",
    response: "Success/failure status."
  },
  "/gotoSiteResume": {
    title: "Resume Navigation",
    desc: "Resumes one or more paused robots. The robots continue executing their current orders from where they stopped.",
    params: "<strong>vehicles</strong> &ndash; array of robot names",
    response: "Success/failure status."
  },
  "/getSimRobotStateTemplate": {
    title: "Simulation State Template",
    desc: "Returns a template JSON structure for simulated robot state. Use this to understand what fields are available when updating sim robots. Only relevant in simulation mode.",
    params: "None",
    response: "Template object with all robot state fields and their default values."
  },
  "/updateSimRobotState": {
    title: "Update Simulated Robot State",
    desc: "Sets the state of a simulated robot (position, battery, status, etc.). Only works when RDS is running in simulation mode. Use for testing order dispatch without real hardware.",
    params: "<strong>vehicle_id</strong> &ndash; simulated robot name<br><strong>battery_percentage</strong> &ndash; battery level (0.0-1.0)<br>Additional fields per getSimRobotStateTemplate",
    response: "Success/failure status."
  },
  "/setContainerGoods": {
    title: "Bind Container Goods",
    desc: "Associates a goods ID with a container on a robot. Used to track what cargo a robot is carrying. The binding persists until explicitly unbound or the goods are delivered.",
    params: "<strong>vehicle</strong> &ndash; robot name<br><strong>containerName</strong> &ndash; container slot on the robot<br><strong>goodsId</strong> &ndash; identifier for the goods being carried",
    response: "Success/failure status."
  },
  "/clearGoods": {
    title: "Unbind Goods",
    desc: "Removes a goods binding by vehicle and goods ID. Use when goods have been delivered or removed from the system.",
    params: "<strong>vehicle</strong> &ndash; robot name<br><strong>goodsId</strong> &ndash; the goods ID to unbind",
    response: "Success/failure status."
  },
  "/clearContainer": {
    title: "Unbind Goods from Container",
    desc: "Removes all goods bindings from a specific container on a specific robot. Use when a container has been emptied.",
    params: "<strong>vehicle</strong> &ndash; robot name<br><strong>containerName</strong> &ndash; container slot name",
    response: "Success/failure status."
  },
  "/clearAllContainersGoods": {
    title: "Clear All Container Goods",
    desc: "Removes all goods bindings from all containers on a single robot. Takes <code>vehicle</code> (singular string), not an array. Use as a reset when container tracking has gotten out of sync.",
    params: "<strong>vehicle</strong> &ndash; robot name (singular, not array)",
    response: "Success/failure status."
  },
  "/getBlockGroup": {
    title: "Occupy Mutex Group",
    desc: "Claims exclusive access to one or more mutex groups. Mutex groups prevent multiple robots from entering the same restricted area simultaneously (e.g. narrow aisles, elevators). Returns a bare JSON array (not wrapped in Response envelope).",
    params: "<strong>id</strong> &ndash; identifier for the occupier (usually robot name)<br><strong>blockGroup</strong> &ndash; array of mutex group names to claim",
    response: "Array of results with name, isOccupied, and occupier fields."
  },
  "/releaseBlockGroup": {
    title: "Release Mutex Group",
    desc: "Releases previously claimed mutex groups, allowing other robots to enter the areas. Returns a bare JSON array.",
    params: "<strong>id</strong> &ndash; must match the original occupier<br><strong>blockGroup</strong> &ndash; array of mutex group names to release",
    response: "Array of results with name, isOccupied, and occupier fields."
  },
  "/blockGroupStatus": {
    title: "Mutex Group Status",
    desc: "Shows the current state of all mutex groups: which are occupied, by whom, and which are free. Returns a bare JSON array. Use for monitoring traffic flow and debugging deadlocks.",
    params: "None",
    response: "Array of mutex groups with name, isOccupied, and occupier."
  },
  "/callTerminal": {
    title: "Call Terminal",
    desc: "Sends a command to an external terminal device (e.g. a conveyor, turntable, or automated door) that is integrated with RDS. The terminal must be pre-configured in the RDS scene.",
    params: "<strong>id</strong> &ndash; terminal ID from scene config<br><strong>type</strong> &ndash; command type (e.g. \"read\", \"write\")",
    response: "Terminal-specific response data."
  },
  "/devicesDetails": {
    title: "Device Details",
    desc: "Lists all external devices configured in the RDS system and their current status. Includes doors, lifts, terminals, and other integrated equipment. Optionally filter by device names.",
    params: "Optional: <strong>devices</strong> query param &ndash; comma-separated device names",
    response: "Object with <code>doors</code>, <code>lifts</code>, and <code>terminals</code> arrays showing name, state, and disabled status."
  },
  "/binDetails": {
    title: "Bin Details",
    desc: "Returns the status of all bin locations (storage positions). Shows which bins are occupied, reserved, or available. Bins are the pickup/dropoff points that robots visit.",
    params: "None &ndash; or pass <strong>binGroups</strong> query param to filter by location group",
    response: "Array of bins with name, group, occupied state, and reservation info."
  },
  "/binCheck": {
    title: "Bin Check",
    desc: "Validates that a list of bin location names exist and are correctly configured in the RDS system. Use to verify location names before creating orders.",
    params: "<strong>bins</strong> &ndash; array of bin location names to check",
    response: "Array of results, each with the bin name and whether it is valid."
  },
  "/callDoor": {
    title: "Call Door",
    desc: "Opens or closes one or more automated doors integrated with RDS. The request body is a JSON array of door commands. Doors are configured as part of the scene.",
    params: "Array of objects: <strong>name</strong> &ndash; door name, <strong>state</strong> &ndash; 1 = open, 0 = close",
    response: "Success/failure status."
  },
  "/disableDoor": {
    title: "Disable Door",
    desc: "Disables or enables automatic door control. When disabled, RDS will not attempt to open the door during robot navigation. Use during maintenance or when manual door control is needed.",
    params: "<strong>names</strong> &ndash; array of door names<br><strong>disabled</strong> &ndash; true to disable, false to re-enable",
    response: "Success/failure status."
  },
  "/callLift": {
    title: "Call Lift",
    desc: "Calls one or more elevators/lifts to specific floors. The request body is a JSON array of lift commands. Used in multi-floor deployments where robots need to move between levels.",
    params: "Array of objects: <strong>name</strong> &ndash; lift name, <strong>target_area</strong> &ndash; destination floor/area",
    response: "Success/failure status."
  },
  "/disableLift": {
    title: "Disable Lift",
    desc: "Disables or enables automatic lift control. When disabled, RDS will not use the lift for robot routing. Use during lift maintenance.",
    params: "<strong>names</strong> &ndash; array of lift names<br><strong>disabled</strong> &ndash; true to disable, false to re-enable",
    response: "Success/failure status."
  },
  "/downloadScene": {
    title: "Download Scene",
    desc: "Downloads the complete scene configuration from RDS as raw binary. The scene defines all locations, paths, zones, devices, and their relationships. Use to back up the configuration.",
    params: "None",
    response: "Raw binary scene data."
  },
  "/uploadScene": {
    title: "Upload Scene",
    desc: "Uploads a new scene configuration to RDS as raw binary. <strong>Warning:</strong> this replaces the entire scene. Typically used for initial setup or major layout changes. Requires a <code>/syncScene</code> call afterward.",
    params: "Raw binary scene data",
    response: "Success/failure status."
  },
  "/syncScene": {
    title: "Sync Scene",
    desc: "Pushes the current scene configuration to all connected robots. Must be called after uploadScene or any scene changes to ensure robots have the latest layout data.",
    params: "None",
    response: "Success/failure status."
  },
  "/scene": {
    title: "Get Scene",
    desc: "Returns the current scene configuration. Lighter-weight than downloadScene &ndash; returns the live in-memory scene as structured JSON rather than raw binary.",
    params: "None",
    response: "Scene object with areas, locations, paths, zones, and bin groups."
  },
  "/getProfiles": {
    title: "Get Configuration Profiles",
    desc: "Retrieves an RDS system configuration file by name. Returns arbitrary config JSON specific to the requested file.",
    params: "<strong>file</strong> &ndash; config file name (e.g. \"properties.json\")",
    response: "Raw JSON configuration content."
  },
  "/ping": {
    title: "Ping",
    desc: "Health check endpoint. Returns product name and version, confirming the RDS service is running and responsive. Use to verify connectivity before sending orders.",
    params: "None",
    response: "Object with <code>product</code> and <code>version</code> fields."
  },
  "/licInfo": {
    title: "License Info",
    desc: "Returns the current RDS license information, including the licensed robot count, expiration date, and feature flags.",
    params: "None",
    response: "License object with maxRobots, expiry, and enabled features array."
  }
};

var prettyMode = true;
var lastResponseBody = null;
var bbBlocks = [];
var bbCounter = 1;

var FIELD_TIPS = {
  blockType: '<strong>Bin Location</strong> &ndash; Go to a named storage bin (e.g. <code>Loc-01</code>) and run its configured action. Best for standard pickup/delivery at defined storage positions.' +
    '<br><br><strong>Map Point + Operation</strong> &ndash; Go to a map waypoint (e.g. <code>AP8</code>) and execute a hardware mechanism like jack lift. For robots with specialized hardware.' +
    '<br><br><strong>Map Point + Script</strong> &ndash; Go to a map waypoint and run a Python script on the robot. For custom automation logic.',
  blockId: 'A unique identifier for this block. Must be globally unique across all orders. Auto-fills as <code>b1</code>, <code>b2</code>, etc. if left blank.' +
    '<br><br>Useful for tracking individual steps &ndash; you can query a block\'s status later with <code>GET /queryBlockDetails?blockId=</code>.',
  location: 'The destination for this block.' +
    '<br><br><strong>For Bin blocks:</strong> a storage bin location name configured in the RDS map, e.g. <code>Loc-01</code>, <code>Loc-02</code>. Use <code>GET /binDetails</code> to see all available bins.' +
    '<br><br><strong>For Map Point blocks:</strong> a map waypoint name, e.g. <code>AP8</code>, <code>AP28</code>. These are defined in the robot map editor.',
  binTask: 'The action to perform at the bin location. These are configured per-bin in the RDS map editor.' +
    '<br><br>Defaults: <code>Load</code> (pick up material) and <code>Unload</code> (drop off material). Your map may define custom tasks beyond these.' +
    '<br><br>For join orders, RDS uses <code>Load</code> for pickup and <code>Unload</code> for delivery automatically.',
  goodsId: 'Optional. A tracking ID for the cargo being moved. If omitted, RDS auto-generates one.' +
    '<br><br>Use this to track specific material through the system. The same goods ID appears in container bindings and order query responses.' +
    '<br><br>Example: a pallet barcode, a batch number, or a Shingo inventory ID.',
  operation: 'The robot mechanism action to execute at the map point. Available operations depend on the robot model and its hardware configuration.' +
    '<br><br>Known values:<br><code>JackLoad</code> &ndash; raise the jack to pick up a pallet/rack<br><code>JackUnload</code> &ndash; lower the jack to set down<br><br>Check your robot\'s "Fixed Path Navigation with Operation" config for the full list of supported operations.',
  opArgs: 'Optional JSON object passed to the operation as parameters.' +
    '<br><br>Example: <code>{"recognize": true}</code> tells a JackLoad to use vision to verify the target before lifting.' +
    '<br><br>Available parameters depend on the operation and robot model.',
  scriptName: 'Filename of a Python script stored on the robot. The robot executes this script when it reaches the location.' +
    '<br><br>Example: <code>firstScript.py</code><br><br>Scripts must be deployed to the robot beforehand via the RDS management tools.',
  scriptArgs: 'Optional JSON object passed to the script as arguments.' +
    '<br><br>Example: <code>{"x": 1.0, "y": 0.0, "coordinate": "robot"}</code>' +
    '<br><br>The script reads these as named parameters. Structure depends on what the script expects.',
  orderId: 'A unique identifier for this order. Can be any string, no length limit. Must not duplicate an existing active order ID.' +
    '<br><br>Tip: use a meaningful prefix like <code>sg-</code> for Shingo-generated orders to distinguish from manual test orders.',
  vehicle: 'Optional. Force-assign this order to a specific robot by name (e.g. <code>AMB-01</code>).' +
    '<br><br>If omitted, RDS dispatches to the best available robot automatically. Use <code>GET /robotsStatus</code> to see available robot names.',
  group: 'Optional. Restrict dispatch to robots in a specific group. Groups are configured in the RDS scene.' +
    '<br><br>Useful when different robot types serve different areas (e.g. a "warehouse" group vs. a "production" group).',
  priority: 'Dispatch priority. Higher numbers are dispatched first. Default is <code>0</code>.' +
    '<br><br>Only matters for orders waiting to be assigned. Once a robot is assigned, priority has no effect.' +
    '<br><br>Use <code>POST /setPriority</code> to change priority on an existing pending order.',
  complete: '<code>true</code> &ndash; all blocks are included now. The order will finish after the last block executes.' +
    '<br><br><code>false</code> &ndash; more blocks will be added later via <code>POST /addBlocks</code>. The robot is assigned immediately and starts executing available blocks. Call <code>POST /markComplete</code> when done adding blocks.' +
    '<br><br>Use <code>false</code> for dynamic routing where the next stop depends on the result of the current one.'
};

function showTip(e, key) {
  e.preventDefault();
  e.stopPropagation();
  var tip = document.getElementById('field-tip');
  var body = document.getElementById('field-tip-body');
  var arrow = tip.querySelector('.field-tip-arrow');

  // If same tip is visible, close it
  if (tip.style.display !== 'none' && tip._activeKey === key) {
    tip.style.display = 'none';
    tip._activeKey = null;
    return;
  }

  body.innerHTML = FIELD_TIPS[key] || '';
  tip._activeKey = key;
  tip.style.display = 'block';

  // Position below the button
  var rect = e.target.getBoundingClientRect();
  var tipRect = tip.getBoundingClientRect();
  var left = rect.left - 12;
  var top = rect.bottom + 8;

  // Keep within viewport
  if (left + tipRect.width > window.innerWidth - 12) left = window.innerWidth - tipRect.width - 12;
  if (left < 8) left = 8;
  if (top + tipRect.height > window.innerHeight - 12) {
    top = rect.top - tipRect.height - 8;
    arrow.style.top = '';
    arrow.style.bottom = '-6px';
    arrow.style.borderBottom = 'none';
    arrow.style.borderTop = '6px solid #1a1a2e';
  } else {
    arrow.style.top = '-6px';
    arrow.style.bottom = '';
    arrow.style.borderTop = 'none';
    arrow.style.borderBottom = '6px solid #1a1a2e';
  }
  arrow.style.left = Math.max(8, rect.left - left + rect.width/2 - 6) + 'px';

  tip.style.left = left + 'px';
  tip.style.top = top + 'px';
}

// Close tooltip when clicking elsewhere
document.addEventListener('click', function(e) {
  if (!e.target.classList.contains('help-btn')) {
    var tip = document.getElementById('field-tip');
    if (tip) { tip.style.display = 'none'; tip._activeKey = null; }
  }
});

function bbTypeChanged() {
  var t = document.getElementById('bb-type').value;
  document.getElementById('bb-bin-fields').style.display = t === 'bin' ? '' : 'none';
  document.getElementById('bb-op-fields').style.display = t === 'operation' ? '' : 'none';
  document.getElementById('bb-script-fields').style.display = t === 'script' ? '' : 'none';
  // Update location placeholder
  var loc = document.getElementById('bb-location');
  loc.placeholder = t === 'bin' ? 'Loc-01' : 'AP8';
}

function bbAddBlock() {
  var t = document.getElementById('bb-type').value;
  var blockId = document.getElementById('bb-blockId').value.trim() || ('b' + bbCounter);
  var location = document.getElementById('bb-location').value.trim();
  if (!location) { alert('Location is required'); return; }

  var block = {blockId: blockId, location: location};
  var label = '';

  if (t === 'bin') {
    block.binTask = document.getElementById('bb-binTask').value;
    var gid = document.getElementById('bb-goodsId').value.trim();
    if (gid) block.goodsId = gid;
    label = block.binTask + ' @ ' + location;
  } else if (t === 'operation') {
    var op = document.getElementById('bb-operation').value.trim();
    if (op) block.operation = op;
    var args = document.getElementById('bb-opArgs').value.trim();
    if (args) { try { block.operationArgs = JSON.parse(args); } catch(e) { alert('Invalid operation args JSON'); return; } }
    label = (op || 'move') + ' @ ' + location;
  } else {
    block.operation = 'Script';
    var sn = document.getElementById('bb-scriptName').value.trim();
    if (sn) block.scriptName = sn;
    var sa = document.getElementById('bb-scriptArgs').value.trim();
    if (sa) { try { block.scriptArgs = JSON.parse(sa); } catch(e) { alert('Invalid script args JSON'); return; } }
    label = 'Script' + (sn ? ': ' + sn : '') + ' @ ' + location;
  }

  bbBlocks.push({block: block, label: label});
  bbCounter++;
  bbRenderBlocks();

  // Reset fields for next block
  document.getElementById('bb-blockId').value = '';
  document.getElementById('bb-location').value = '';
  document.getElementById('bb-goodsId').value = '';
  document.getElementById('bb-opArgs').value = '';
  document.getElementById('bb-scriptArgs').value = '';
}

function bbRemoveBlock(idx) {
  bbBlocks.splice(idx, 1);
  bbRenderBlocks();
}

function bbRenderBlocks() {
  var el = document.getElementById('bb-blocks');
  if (bbBlocks.length === 0) { el.innerHTML = '<span class="text-muted" style="font-size:0.78rem">No blocks added yet. Add blocks above, then click Generate JSON.</span>'; return; }
  var html = '';
  for (var i = 0; i < bbBlocks.length; i++) {
    html += '<div class="bb-row"><span class="bb-num">' + (i+1) + '</span><span class="bb-detail">' +
      bbBlocks[i].block.blockId + ' &ndash; ' + bbBlocks[i].label +
      '</span><button class="bb-rm" onclick="bbRemoveBlock(' + i + ')" title="Remove">&times;</button></div>';
  }
  el.innerHTML = html;
}

function bbGenerate() {
  if (bbBlocks.length === 0) { alert('Add at least one block first'); return; }
  var order = {
    id: document.getElementById('bb-orderId').value.trim() || '',
    complete: document.getElementById('bb-complete').value === 'true',
    priority: parseInt(document.getElementById('bb-priority').value) || 0,
    blocks: bbBlocks.map(function(b) { return b.block; })
  };
  var v = document.getElementById('bb-vehicle').value.trim();
  if (v) order.vehicle = v;
  var g = document.getElementById('bb-group').value.trim();
  if (g) order.group = g;
  document.getElementById('req-body').value = JSON.stringify(order, null, 2);
}

function toggleBlockBuilder() {
  var form = document.getElementById('bb-form');
  var order = document.getElementById('bb-order');
  var btn = document.getElementById('bb-toggle');
  if (form.style.display === 'none') {
    form.style.display = '';
    order.style.display = '';
    btn.textContent = 'Hide Builder';
  } else {
    form.style.display = 'none';
    order.style.display = 'none';
    btn.textContent = 'Show Builder';
  }
}

function showBlockBuilder(show) {
  document.getElementById('block-builder').style.display = show ? '' : 'none';
  if (show) bbRenderBlocks();
}

function filterEndpoints() {
  var q = document.getElementById('ep-search').value.toLowerCase();
  document.querySelectorAll('.ep-item').forEach(function(el) {
    el.style.display = el.textContent.toLowerCase().indexOf(q) >= 0 ? '' : 'none';
  });
  document.querySelectorAll('.ep-group').forEach(function(el) {
    var next = el.nextElementSibling;
    var anyVisible = false;
    while (next && !next.classList.contains('ep-group')) {
      if (next.style.display !== 'none') anyVisible = true;
      next = next.nextElementSibling;
    }
    el.style.display = anyVisible || !q ? '' : 'none';
  });
}

function loadEP(el, method, path, title, bodyTemplate) {
  document.getElementById('req-method').value = method;
  document.getElementById('req-path').value = path;
  document.getElementById('req-title').textContent = title;
  document.getElementById('req-body').value = bodyTemplate || '';
  document.getElementById('req-body-section').style.display = method === 'POST' ? '' : 'none';
  document.querySelectorAll('.ep-item').forEach(function(e) { e.classList.remove('ep-active'); });
  el.classList.add('ep-active');
  // Show block builder for setOrder and addBlocks
  showBlockBuilder(path === '/setOrder' || path === '/addBlocks');
}

function showInfo(e, path) {
  e.stopPropagation();
  var info = EP_INFO[path];
  if (!info) return;
  document.getElementById('info-title').textContent = info.title;
  var html = '<p>' + info.desc + '</p>';
  if (info.params) html += '<p><strong>Parameters:</strong><br>' + info.params + '</p>';
  if (info.response) html += '<p><strong>Response:</strong> ' + info.response + '</p>';
  document.getElementById('info-body').innerHTML = html;
  document.getElementById('info-overlay').classList.add('active');
}

function closeInfo(e) {
  if (e && e.target !== document.getElementById('info-overlay')) return;
  document.getElementById('info-overlay').classList.remove('active');
}

function sendRequest() {
  var method = document.getElementById('req-method').value;
  var path = document.getElementById('req-path').value;
  var body = document.getElementById('req-body').value.trim();
  var btn = document.getElementById('send-btn');

  btn.textContent = 'Sending...';
  btn.disabled = true;

  fetch('/api/fleet/proxy', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({method: method, path: path, body: body})
  })
  .then(function(r) { return r.json(); })
  .then(function(data) {
    btn.textContent = 'Send';
    btn.disabled = false;
    showResponse(data);
  })
  .catch(function(err) {
    btn.textContent = 'Send';
    btn.disabled = false;
    showResponse({error: err.message, status_code: 0, elapsed_ms: 0});
  });
}

function showResponse(data) {
  document.getElementById('resp-actions').style.display = '';

  var status = document.getElementById('resp-status');
  var code = data.status_code || 0;
  status.textContent = code ? code + ' ' + httpStatusText(code) : 'Error';
  status.className = 'badge ' + (code >= 200 && code < 300 ? 'badge-completed' : 'badge-failed');

  document.getElementById('resp-time').textContent = data.elapsed_ms + 'ms' + (data.url ? ' | ' + data.method + ' ' + data.url : '');

  if (data.error && !data.body) {
    lastResponseBody = {error: data.error};
  } else if (data.body !== undefined) {
    lastResponseBody = data.body;
  } else if (data.body_text !== undefined) {
    lastResponseBody = data.body_text;
  } else {
    lastResponseBody = data;
  }
  renderBody();

  if (data.headers) {
    document.getElementById('resp-headers').textContent = JSON.stringify(data.headers, null, 2);
  }
}

function renderBody() {
  var el = document.getElementById('resp-body');
  if (typeof lastResponseBody === 'string') {
    el.textContent = lastResponseBody;
  } else {
    el.textContent = prettyMode ? JSON.stringify(lastResponseBody, null, 2) : JSON.stringify(lastResponseBody);
  }
}

function togglePretty() {
  prettyMode = !prettyMode;
  document.getElementById('pretty-btn').textContent = prettyMode ? 'Compact' : 'Pretty';
  renderBody();
}

function copyResponse() {
  var text = document.getElementById('resp-body').textContent;
  navigator.clipboard.writeText(text);
}

function toggleHeaders() {
  var el = document.getElementById('resp-headers');
  el.style.display = el.style.display === 'none' ? '' : 'none';
}

function httpStatusText(code) {
  var codes = {200:'OK',201:'Created',204:'No Content',400:'Bad Request',401:'Unauthorized',403:'Forbidden',404:'Not Found',500:'Server Error',502:'Bad Gateway',503:'Unavailable'};
  return codes[code] || '';
}

document.getElementById('req-method').addEventListener('change', function() {
  document.getElementById('req-body-section').style.display = this.value === 'POST' ? '' : 'none';
});

document.addEventListener('keydown', function(e) {
  if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') sendRequest();
  if (e.key === 'Escape') document.getElementById('info-overlay').classList.remove('active');
});
