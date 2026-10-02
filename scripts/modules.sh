# The repository's modules, for the release scripts to source: one list, so
# what is tagged, what is compared and what is fetched cannot drift apart.

# Library modules beside the root: tagged <dir>/vX.Y.Z in lockstep with it.
LIB_MODULES=(mcp models/anthropic sandbox/docker sessions skills)

# Main modules: built from the repository, never tagged.
APP_MODULES=(cmd/agents-server examples/anthropic)
