// Synthetic fixture for the registry parser — exercises every resolver path.
// Mirrors the shape of Codebuff's real free-agents.ts / freebuff-models.ts
// (typed consts, alias chains, `as const`, object constants, Set constants,
// computed root-map keys). Alias lines intentionally carry no trailing
// semicolon: the JS alias regex anchors `$` to the line end and the parser
// must stay behavior-identical to it.

export const MODEL_ALPHA = 'alpha/model-one';
export const MODEL_BETA = 'beta/model-two';
export const MODEL_UNUSED = 'gamma/unused-model';
export const LITERAL_LEAF = 'chain/leaf';

// Alias forms: plain, `as const`, chained.
export const ALIAS_ONE = MODEL_ALPHA
export const ALIAS_TWO = MODEL_BETA as const
export const ALIAS_CHAIN = ALIAS_ONE

// A chain deeper than the 8-hop cap: must not resolve.
export const CHAIN_DEEP_1 = LITERAL_LEAF
export const CHAIN_DEEP_2 = CHAIN_DEEP_1
export const CHAIN_DEEP_3 = CHAIN_DEEP_2
export const CHAIN_DEEP_4 = CHAIN_DEEP_3
export const CHAIN_DEEP_5 = CHAIN_DEEP_4
export const CHAIN_DEEP_6 = CHAIN_DEEP_5
export const CHAIN_DEEP_7 = CHAIN_DEEP_6
export const CHAIN_DEEP_8 = CHAIN_DEEP_7
export const CHAIN_DEEP_9 = CHAIN_DEEP_8
export const CHAIN_DEEP_10 = CHAIN_DEEP_9

// Non-string constants: never aliases (matches the JS lookahead exclusions).
export const ENABLED = true;
export const DISABLED = false;
export const NOTHING = null;
export const COUNT = 42;

// Object constant + dotted alias.
export const agentConfig = {
  primary: 'object/model-one',
  backup: 'object/model-two',
}
export const OBJ_PROP = agentConfig.primary

// Set constant mixing literal and identifier members.
export const AGENT_SET = new Set<string>([
  'set/literal-one',
  ALIAS_CHAIN,
  MODEL_UNUSED,
])

// Root map: computed const keys (wins over first-seen agent assignment).
// Unresolvable keys are skipped: CHAIN_DEEP_10 needs 10 alias hops, past the
// 8-hop cap, and NOT_A_CONST does not exist.
export const FREEBUFF_ROOT_AGENT_ID_BY_MODEL: Record<string, string> = {
  [MODEL_ALPHA]: 'root-alpha',
  [CHAIN_DEEP_10]: 'root-chain',
  [NOT_A_CONST]: 'root-ghost',
}

// Agent models: inline sets (literals + identifiers) and a named Set constant.
export const FREE_MODE_AGENT_MODELS: Record<string, Set<string>> = {
  'free-agent-alpha': new Set([MODEL_ALPHA, 'alpha/model-extra']),
  'free-agent-beta': AGENT_SET,
  'free-agent-gamma': new Set([ALIAS_TWO]),
  'free-agent-delta': new Set([OBJ_PROP]),
}