import type { UpstreamFactory } from '@gunnflow/upstream-port';
import { createRhizomeUpstream } from './upstream.js';

export { createRhizomeUpstream } from './upstream.js';
export { adaptWorkspaceBody } from './workspaceWire.js';
export { projectRhizomeNodes, WORKSPACE_NODE_ID } from './nodes.js';
export { adaptEnvelope, adaptWithReport, contractRefusal, nodesOf } from './upstream.js';
export type { RhizomeIntegrationReport } from './nodes.js';
export { SseDecoder } from './upstream.js';
export { detailOf, detailItems } from './details.js';
export { GATE_BODY_LABELS, parseGateBodySections, stripMarkdown } from './gateBody.js';
export { executionForWire, executionTaskProblem, projectExecution } from './execution.js';
export type { RhizomeExecBody, RhizomeExecEvent, RhizomeExecSession } from './execution.js';

const DEFAULT_URL = 'http://127.0.0.1:8790';

/**
 * Contract version this adapter conforms to (checked by the composition root).
 * 0.2.0 = node detail surface (RHZ-072); 0.3.0 = execution surface on the
 * direct wire (RHZ-094, FR-RHZ-121). 0.x is minor-strict: 0.2.x claims are
 * rejected by a 0.3.0 Gunnflow. 0.3.1 only widens the package's vitest peer
 * range; the wire is unchanged.
 */
export const contractVersion = '0.3.1';

/** Entry the BFF composition root loads by module name. */
export const createUpstream: UpstreamFactory = (options) => createRhizomeUpstream(options.url ?? DEFAULT_URL);
