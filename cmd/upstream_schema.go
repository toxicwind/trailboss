package main
func upstreamStatusSchemaObj() map[string]any {
	return object(map[string]any{
		"fork":         str(),
		"repo":         str(),
		"behind":       integer(),
		"ahead":        integer(),
		"upstream_sha": str(),
		"needs_merge":  boolean(),
	}, "fork", "repo", "behind", "ahead", "upstream_sha", "needs_merge")
}

func upstreamMergeResultSchemaObj() map[string]any {
	return object(map[string]any{
		"fork":             str(),
		"upstream_new_sha": str(),
		"upstream_commits": integer(),
		"branch":           str(),
		"clean":            boolean(),
		"conflicts":        arrayOf(str()),
		"tests_passed":     boolean(),
		"test_output":      str(),
		"ready":            boolean(),
		"report":           str(),
	}, "fork", "upstream_new_sha", "upstream_commits", "branch", "clean", "tests_passed", "ready", "report")
}

func upstreamWatchResultSchemaObj() map[string]any {
	return object(map[string]any{
		"at":      dateTime(),
		"results": arrayOf(upstreamMergeResultSchemaObj()),
		"summary": str(),
	}, "at", "results", "summary")
}
