package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIToolsFilter_ResponsesKeepsOnlyFunctionTools(t *testing.T) {
	body := []byte(`{
		"model": "model1",
		"input": "hello",
		"tools": [
			{"type":"function","name":"kept","parameters":{}},
			{"type":"web_search"},
			{"type":"image_generation","output_format":"png"},
			{"type":"tool_search","execution":"sync"},
			{"type":"namespace","name":"mcp__server","tools":[]}
		]
	}`)

	filtered, removed, err := filterUnsupportedOpenAITools(body, "/v1/responses")
	require.NoError(t, err)
	assert.Equal(t, 4, removed)
	assert.Equal(t, int64(1), gjson.GetBytes(filtered, "tools.#").Int())
	assert.Equal(t, "function", gjson.GetBytes(filtered, "tools.0.type").String())
	assert.Equal(t, "kept", gjson.GetBytes(filtered, "tools.0.name").String())
}

func TestOpenAIToolsFilter_ChatKeepsOnlyFunctionTools(t *testing.T) {
	body := []byte(`{
		"model": "model1",
		"messages": [],
		"tools": [
			{"type":"function","function":{"name":"kept","parameters":{}}},
			{"type":"web_search"},
			{"type":"namespace","name":"mcp__server","tools":[]}
		]
	}`)

	filtered, removed, err := filterUnsupportedOpenAITools(body, "/v1/chat/completions")
	require.NoError(t, err)
	assert.Equal(t, 2, removed)
	assert.Equal(t, int64(1), gjson.GetBytes(filtered, "tools.#").Int())
	assert.Equal(t, "function", gjson.GetBytes(filtered, "tools.0.type").String())
	assert.Equal(t, "kept", gjson.GetBytes(filtered, "tools.0.function.name").String())
}

func TestOpenAIToolsFilter_RemovesToolsWhenNoFunctionToolsRemain(t *testing.T) {
	body := []byte(`{"model":"model1","input":"hello","tools":[{"type":"web_search"},{"type":"namespace","tools":[]}]}`)

	filtered, removed, err := filterUnsupportedOpenAITools(body, "/v1/responses")
	require.NoError(t, err)
	assert.Equal(t, 2, removed)
	assert.False(t, gjson.GetBytes(filtered, "tools").Exists())
}

func TestOpenAIToolsFilter_LeavesAnthropicMessagesToolsAlone(t *testing.T) {
	body := []byte(`{"model":"model1","tools":[{"name":"read_file","input_schema":{"type":"object"}}]}`)

	filtered, removed, err := filterUnsupportedOpenAITools(body, "/v1/messages")
	require.NoError(t, err)
	assert.Equal(t, 0, removed)
	assert.Equal(t, string(body), string(filtered))
}
