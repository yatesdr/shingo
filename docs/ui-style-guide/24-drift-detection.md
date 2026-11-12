## Drift detection

Verified against: 51489c78

Every `*_drift_test.go` in the workspace's modules, each test with the first
sentence of its doc comment. A rule chapter names the guard that holds it; this
is the whole set.

<!-- generated: *_drift_test.go -->

| File | Test | What it pins |
|---|---|---|
| `integration/scenarios/order_projection_drift_test.go` | `TestOrderProjection_NoFieldDropsSilently` | TestOrderProjection_NoFieldDropsSilently is the gate. |
| `integration/scenarios/order_projection_drift_test.go` | `TestOrderProjection_KnownDroppedAreStillDropped` | TestOrderProjection_KnownDroppedAreStillDropped is the other direction, and it is what stops knownDropped from rotting into a list of things that used to be true. |
| `protocol/claim_geometry_drift_test.go` | `TestClaimGeometryHasOneDerivationPoint` | TestClaimGeometryHasOneDerivationPoint walks every production file in every module and fails on any []string literal that names both index positions. |
| `protocol/docs_drift_test.go` | `TestWireProtocolDocNoExpiryProseCountsTheSet` | docs/wire-protocol.md's subject registry, TTL tables, type registry and payload field tables are GENERATED from this package (shared/scripts/docgen, wire.go), and docgen's TestGeneratedBlocksAreCurrent fails when a committed block is not what the code emits now. |
| `protocol/loader_derivation_drift_test.go` | `TestLoaderQuestionHasOneDerivationPoint` | TestLoaderQuestionHasOneDerivationPoint walks every production file in every module and fails on any reference to the manual_swap constant that is not accounted for above. |
| `protocol/terminal_status_sql_drift_test.go` | `TestTerminalStatusSQLHasOneSpelling` | TestTerminalStatusSQLHasOneSpelling refuses a hand-spelled copy of the terminal-status set in SQL anywhere outside protocol/. |
| `protocol/transitions_doc_drift_test.go` | `TestTransitionsDocPointsAtLiveSymbols` | docs/order-state-machine/transitions.md deliberately does NOT copy the state machine out of the code — it is a table of POINTERS: "the statuses live in protocol/status.go", "the transitions live in validTransitions", and so on. |
| `protocol/transitions_doc_drift_test.go` | `TestTransitionsDocPredicateListIsComplete` | TestTransitionsDocPredicateListIsComplete asserts the doc's predicate list equals the exported Is\*/Blocks\* predicates in protocol/status.go — both directions: a predicate missing from the doc fails, and a doc entry with no predicate fails. |
| `protocol/transitions_doc_drift_test.go` | `TestTransitionsDocTableMatchesTheCode` | TestTransitionsDocTableMatchesTheCode pins the doc's transition table to protocol's: the rows must be exactly AllStatuses(), each non-terminal row's targets exactly that status's validTransitions edges, and each terminal row (no key in the table) marked \*(terminal)\*. |
| `protocol/wire_vocabulary_drift_test.go` | `TestWireVocabularyHasOneDefinitionSite` | — |
| `protocol/wire_vocabulary_drift_test.go` | `TestOutboxRetryCapHasOneDefinitionSite` | TestOutboxRetryCapHasOneDefinitionSite is the same guard for the retry cap, which was an integer rather than a string and so needs its own scan. |
| `shared/identity_hue_drift_test.go` | `TestIdentityHuesAreDeclaredOnce` | — |
| `shared/identity_hue_drift_test.go` | `TestStyleGuideQuotesTheIdentityHues` | TestStyleGuideQuotesTheIdentityHues holds the guide's token block to tokens.css, value for value. |
| `shared/identity_hue_drift_test.go` | `TestSurfaceInkTokensAreDeclared` | — |
| `shared/identity_hue_drift_test.go` | `TestStyleGuideQuotesTheSurfaceInkTokens` | — |
| `shared/identity_hue_drift_test.go` | `TestSurfaceInkTokensAreReadByBothAdminSurfaces` | TestSurfaceInkTokensAreReadByBothAdminSurfaces is the half that keeps the literal from coming back. |
| `shared/identity_hue_drift_test.go` | `TestTheStationPictureCarriesNoLiteralColour` | TestTheStationPictureCarriesNoLiteralColour: the exception is retired. |
| `shared/status_drift_test.go` | `TestStatusClassesCoversAllProtocolStatuses` | TestStatusClassesCoversAllProtocolStatuses asserts that every value in protocol.AllStatuses() has a corresponding .badge-&lt;status&gt; rule in shared/status-classes.css. |
| `shared/status_drift_test.go` | `TestStatusClassesNoStrayBadgeClasses` | TestStatusClassesNoStrayBadgeClasses asserts the inverse: no .badge-&lt;x&gt; class in the file references a status that isn't in protocol.AllStatuses(). |
| `shared/status_drift_test.go` | `TestTokensCSSHasRequiredSemantic` | TestTokensCSSHasRequiredSemantic ensures the design tokens the style guide promises are all defined. |
| `shared/status_drift_test.go` | `TestTokensCSSInfoDistinctFromPrimary` | TestTokensCSSInfoDistinctFromPrimary holds --info and --primary to distinct colours in both themes, so an info callout never reads as a primary CTA. |
| `shared/status_drift_test.go` | `TestSourcingBadgeIsQueuedsAlias` | TestSourcingBadgeIsQueuedsAlias: `sourcing` has no badge of its own. |
| `shingo-core/store/burial_exclusion_drift_test.go` | `TestBurialExclusionOneSpelling` | TestBurialExclusionOneSpelling holds "is this slot in front of a bin somebody is coming for" to one spelling. |
| `shingo-core/store/demand_tense_drift_test.go` | `TestDemandTense_NoQueryDecidesTenseOutsideTheHelpers` | TestDemandTense_NoQueryDecidesTenseOutsideTheHelpers is the census and the walk described at the top of the file. |
| `shingo-core/store/internal/helpers/lane_reachability_drift_test.go` | `TestReachabilityHasExactlyOneSpelling` | TestReachabilityHasExactlyOneSpelling is the point of the whole exercise, made mechanical. |
| `shingo-core/store/one_clock_drift_test.go` | `TestTimelineInsertsStampTheAppClock` | one_clock_drift_test.go — EVERYTHING FOLLOWS THE SAME CLOCK. |
| `shingo-core/store/orders/demand_rank_drift_test.go` | `TestNoThirdSpellingOfTheDemandRanking` | TestNoThirdSpellingOfTheDemandRanking refuses a hand-spelled demand ordering (priority, then oldest) anywhere but rankHome: blocking_drift_test's guard applied to the demand-ranking seam. |
| `shingo-core/store/orders/demand_rank_drift_test.go` | `TestDemandRankOrderBySQL_IsTotal` | TestDemandRankOrderBySQL_IsTotal pins the tiebreak, which the source scan above cannot see: it counts SPELLINGS, and a spelling that stops at (priority, created_at) is still exactly one. |
| `shingo-core/store/reconciliation/acquiring_spelling_drift_test.go` | `TestAcquiringSetHasExactlyOneSpelling` | TestAcquiringSetHasExactlyOneSpelling is blocking_drift_test's guard applied to the other set this codebase keeps two definitions of. |
| `shingo-core/store/reservations/blocking_drift_test.go` | `TestBlockingPredicateHasExactlyOneSpelling` | TestBlockingPredicateHasExactlyOneSpelling is dig_exclusion's guard applied to the question underneath it: does a reservation row on this resource make the resource unavailable? |
| `shingo-core/store/reservations/dig_exclusion_drift_test.go` | `TestDigExclusionHasExactlyOneSQLSpelling` | TestDigExclusionHasExactlyOneSQLSpelling is the reachability guard's twin (store/internal/helpers/lane_reachability_drift_test.go), for the same reason and against the same failure mode. |
| `shingo-core/store/reservations/dig_exclusion_drift_test.go` | `TestDigOwnerComparisonHasExactlyOneSpelling` | TestDigOwnerComparisonHasExactlyOneSpelling is the Go-side half. |
| `shingo-core/store/schema_index_drift_test.go` | `TestBaselineDDL_DoesNotIndexMigrationAddedColumns` | — |
| `shingo-core/store/schema_index_drift_test.go` | `TestSchemaDriftParsers_MatchTheShapesInUse` | The parsers are the load-bearing part — a regex that quietly stops matching turns the check above into a no-op that always passes. |
| `shingo-core/www/chip_drift_test.go` | `TestEveryChipModifierHasAStylesheetRule` | TestEveryChipModifierHasAStylesheetRule asserts that every `.chip-<x>` applied alongside the base `.chip` class in a template or page JS file has a rule in some stylesheet this surface loads. |
| `shingo-core/www/clock_globals_drift_test.go` | `TestEveryClockPageCarriesGlobals` | TestEveryClockPageCarriesGlobals is Core's half of the shared rule: a full page whose scripts reach a clock function in shared/clock.js must carry window.PLANT_TZ and window.SHINGO_CLOCK. |
| `shingo-core/www/clock_globals_drift_test.go` | `TestNoPageModuleReadsTheBrowserCalendar` | TestNoPageModuleReadsTheBrowserCalendar: a page module may not take a date or time of day from the browser's own zone. |
| `shingo-core/www/clock_globals_drift_test.go` | `TestNoPageModuleDefinesADurationFormatter` | TestNoPageModuleDefinesADurationFormatter: there is one duration ladder, shared/clock.js formatDuration (the twin of protocol.FormatDuration, guide "Durations are compound"). |
| `shingo-core/www/clock_globals_drift_test.go` | `TestEscapeHtmlDefinedOnce` | TestEscapeHtmlDefinedOnce: the HTML escape is defined in one file across shared/ and Core's static tree — shared/dom.js, the copy that escapes quotes (pin: shared/testdata/js/utils.escape.test.js). |
| `shingo-core/www/clock_globals_drift_test.go` | `TestPlantTZReadInTwoFiles` | TestPlantTZReadInTwoFiles: the plant zone is read in exactly two browser files — shared/clock.js (formatTime, formatClock) and Core's components/plantclock.js (plant date, seconds, windows, bucket labels). |
| `shingo-core/www/clock_globals_drift_test.go` | `TestPlantClockJS` | TestPlantClockJS runs plantclock.test.js: plantclock.js agrees with the shared formatClock / formatTime on a vector table (UTC, America/Chicago, Asia/Tokyo, both DST edges, midnights), and resolves windows from the server's now on the plant calendar. |
| `shingo-core/www/clock_globals_drift_test.go` | `TestNoChartSetsItsOwnSmoothing` | TestNoChartSetsItsOwnSmoothing: lines are straight (guide "Time series"). |
| `shingo-core/www/css_collision_drift_test.go` | `TestNoBareClassDeclaredInTwoCoLoadedStylesheets` | — |
| `shingo-core/www/emoji_drift_test.go` | `TestNoEmojiInTemplatesAndPageJS` | TestNoEmojiInTemplatesAndPageJS enforces the "no emoji, ever" icon policy (see docs/ui-style-guide.md § Icons). |
| `shingo-core/www/fleet_message_render_drift_test.go` | `TestFleetMessagesAreRenderedAsMessages` | TestFleetMessagesAreRenderedAsMessages asserts that every page rendering the fleet's errors / warnings / notices reads the fields off them, rather than stringifying the object. |
| `shingo-core/www/inline_onclick_drift_test.go` | `TestNoInlineEventHandlersInTemplates` | TestNoInlineEventHandlersInTemplates pins the "no inline event handlers" convention from the style guide (§ Event handling). |
| `shingo-core/www/inline_onclick_drift_test.go` | `TestNoMalformedDataActionInJS` | TestNoMalformedDataActionInJS refuses a stray ")" or a literal backslash-quote \' inside a data-action attribute value built by string concatenation in the page JS, the two shapes an onclick="fn(arg)" -&gt; data-action="fn:arg" rewrite leaves. |
| `shingo-core/www/js_status_maps_drift_test.go` | `TestJSStatusTablesAreKeyedOnRealStatuses` | — |
| `shingo-core/www/js_status_maps_drift_test.go` | `TestTestOrdersIsActiveIsTheNonTerminalSet` | — |
| `shingo-core/www/mission_state_vocabulary_drift_test.go` | `TestMissionDetailStageClassIsTheStatusSet` | TestMissionDetailStageClassIsTheStatusSet pins the stage table's class map to the protocol status enum, in both directions, and the four classes to their labels and colours. |
| `shingo-core/www/mission_state_vocabulary_drift_test.go` | `TestMissionDetailCarriesNoVendorStateMap` | TestMissionDetailCarriesNoVendorStateMap fails if the page starts translating vendor states again. |
| `shingo-core/www/node_algorithm_vocabulary_drift_test.go` | `TestNodeAlgorithmVocabularyMatchesResolver` | TestNodeAlgorithmVocabularyMatchesResolver pins the nodes page to the resolver's constants: the retrieve/store algorithm codes the page offers and the node-property keys it reads and writes. |
| `shingo-core/www/template_var_drift_test.go` | `TestNoUndefinedCSSVarsInTemplates` | TestNoUndefinedCSSVarsInTemplates guards the blind spot that let a `var(--card-bg)` reference sit in a template long after the token was renamed away — it rendered as unset with no error. |
| `shingo-core/www/undeclared_carrier_watcher_drift_test.go` | `TestInventoryPageReadsTheUndeclaredCarrierCount` | undeclared_carrier_watcher_drift_test.go — the count history page's half of the carrier-rule watcher, held as source text. |
| `shingo-core/www/zlayer_drift_test.go` | `TestNoRawZIndexInCoreAssets` | TestNoRawZIndexInCoreAssets holds every stylesheet, page script and template this surface ships to the layer scale in shared/tokens.css. |
| `shingo-edge/domain/flow_preset_word_drift_test.go` | `TestSwapModeWordsMatchTheModel` | TestSwapModeWordsMatchTheModel holds SwapModeWord to composer-model.js's MODES block, which is the authority: both surfaces render their chips and cards from it, and the server only needs the words to build a preset's suggested name. |
| `shingo-edge/domain/flow_preset_word_drift_test.go` | `TestSwapModeWordsAreTitleCase` | TestSwapModeWordsAreTitleCase is which words the two tables hold: title case, "1‑Robot Swap" and never "1 robot swap". |
| `shingo-edge/domain/flow_preset_word_drift_test.go` | `TestShapeFieldsAreRealClaimFields` | TestShapeFieldsAreRealClaimFields holds the rule A FIELD IS CALLED WHAT THE CLAIM CALLS IT; INVENT NO SECOND NAME. |
| `shingo-edge/domain/flow_preset_word_drift_test.go` | `TestShapeFieldsMatchTheModel` | TestShapeFieldsMatchTheModel holds the two LISTS together: the same fields, in the same order, on both surfaces. |
| `shingo-edge/store/claim_live_predicate_drift_test.go` | `TestClaimReads_CarryTheLivePredicate` | claim_live_predicate_drift_test.go — every READ of style_node_claims that decides live behaviour must exclude retired rows. |
| `shingo-edge/store/claim_live_predicate_drift_test.go` | `TestRawStringLiterals_SkipsComments` | TestRawStringLiterals_SkipsComments: the scanner reads code, not prose. |
| `shingo-edge/store/processes/node_generation_drift_test.go` | `TestProcessNodeWritersPublishTheirGeneration` | node_generation_drift_test.go — every writer of process_nodes publishes that it wrote. |
| `shingo-edge/www/clock_globals_drift_test.go` | `TestEveryClockPageCarriesGlobals` | TestEveryClockPageCarriesGlobals is Edge's half of the shared rule: a full page whose scripts reach a clock function in shared/clock.js must carry window.PLANT_TZ and window.SHINGO_CLOCK. |
| `shingo-edge/www/composer_swaps_drift_test.go` | `TestComposerCharacterizationSwapsAreProtocolModes` | TestComposerCharacterizationSwapsAreProtocolModes pins the Flow Composer characterization suite's SWAPS list to protocol.AllSwapModes(). |
| `shingo-edge/www/composer_vocabulary_drift_test.go` | `TestComposerSurfacesSpellNoFieldWordOfTheirOwn` | composer_vocabulary_drift_test.go — A FIELD IS CALLED WHAT THE CLAIM CALLS IT, and there is ONE table of those words (domain/flowspec). |
| `shingo-edge/www/composer_vocabulary_drift_test.go` | `TestRoutingRolesReadInboundAndOutbound` | TestRoutingRolesReadInboundAndOutbound: the routing set's three roles read Inbound, Staging and Outbound on the Processes page. |
| `shingo-edge/www/composer_vocabulary_drift_test.go` | `TestFlowspecLabelsReachTheStation` | TestFlowspecLabelsReachTheStation: the generated block the model reads carries the labels, so fieldWord has something to look up. |
| `shingo-edge/www/emoji_drift_test.go` | `TestNoEmojiInTemplatesAndPageJS` | TestNoEmojiInTemplatesAndPageJS enforces the "no emoji, ever" icon policy (see docs/ui-style-guide.md § Icons) on the Edge admin surface. |
| `shingo-edge/www/inline_onclick_drift_test.go` | `TestNoInlineEventHandlersInTemplates` | TestNoInlineEventHandlersInTemplates pins the "no inline event handlers" convention from the style guide (§ Event handling). |
| `shingo-edge/www/modal_scroll_drift_test.go` | `TestModalDeclaresScrollContainment` | TestModalDeclaresScrollContainment holds .modal to a max-height and a flex column, so a modal taller than the viewport scrolls inside itself and its footer — and therefore Save — stays on screen. |
| `shingo-edge/www/order_status_js_drift_test.go` | `TestOrderStatusJSAgreesWithProtocol` | TestOrderStatusJSAgreesWithProtocol pins the JS-side status arrays in static/operator-station/order-status.js to the Go-side projectors in protocol/status.go. |
| `shingo-edge/www/order_status_js_drift_test.go` | `TestWindowStateActiveStatusesAgreeWithProtocol` | TestWindowStateActiveStatusesAgreeWithProtocol pins operator-window-state.js's WINDOW_ACTIVE_STATUSES array against the protocol's non-terminal set. |
| `shingo-edge/www/processes_enum_drift_test.go` | `TestComposerModesMatchProtocol` | TestComposerModesMatchProtocol pins composer-model.js's MODES — the mode picker's rows, and the labels the rail and the positions table read — to protocol.ConfigurableSwapModes() minus manual_swap. |
| `shingo-edge/www/processes_enum_drift_test.go` | `TestComposerRolesDerivedThenSettable` | TestComposerRolesDerivedThenSettable: role starts derived — the prior claim on the node, then the majority of the style's other claims, then the majority of the process's, then consume — and an engineer can set it by hand when the press does the other thing (setRole). |
| `shingo-edge/www/template_dup_attr_drift_test.go` | `TestTemplatesHaveNoDuplicateAttributes` | A DUPLICATE ATTRIBUTE IS INVISIBLE AND LOSSY. |
| `shingo-edge/www/template_field_drift_test.go` | `TestTemplateRuntimeFieldsExistOnDomainType` | A template's field references are invisible to every gate the repo runs. |
| `shingo-edge/www/zlayer_drift_test.go` | `TestNoRawZIndexInEdgeAssets` | TestNoRawZIndexInEdgeAssets holds Edge's stylesheets, page scripts and templates to the layer scale in shared/tokens.css. |
| `shingo-edge/www/zlayer_drift_test.go` | `TestOperatorStationZLayerScaleMatchesShared` | TestOperatorStationZLayerScaleMatchesShared holds the operator station's copy of the layer scale to the shared one. |

<!-- /generated -->
