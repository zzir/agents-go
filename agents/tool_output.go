package agents

import (
	"encoding/base64"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/internal/oaiitems"
)

// ToolOutputContent is one content part of a multimodal tool result (native
// text, image or file input); a tool returns one, or a []ToolOutputContent.
// Sealed: ToolOutputText, ToolOutputImage and ToolOutputFile.
type ToolOutputContent interface {
	isToolOutputContent()
	toContentParam() responses.ResponseFunctionCallOutputItemUnionParam
}

// ToolOutputText is a plain-text content part, combinable with images and files.
type ToolOutputText struct {
	Text string
}

func (ToolOutputText) isToolOutputContent() {}

func (t ToolOutputText) toContentParam() responses.ResponseFunctionCallOutputItemUnionParam {
	return responses.ResponseFunctionCallOutputItemParamOfInputText(t.Text)
}

// ToolOutputImageDetail is the requested fidelity of a ToolOutputImage.
type ToolOutputImageDetail string

// The predefined image-detail levels.
const (
	DetailLow      ToolOutputImageDetail = "low"
	DetailHigh     ToolOutputImageDetail = "high"
	DetailAuto     ToolOutputImageDetail = "auto"
	DetailOriginal ToolOutputImageDetail = "original"
)

// ToolOutputImage is an image content part: exactly one of ImageURL (a URL or
// data: URL) or FileID (an uploaded OpenAI file); Detail is optional.
type ToolOutputImage struct {
	ImageURL string
	FileID   string
	Detail   ToolOutputImageDetail
}

func (ToolOutputImage) isToolOutputContent() {}

func (im ToolOutputImage) toContentParam() responses.ResponseFunctionCallOutputItemUnionParam {
	p := responses.ResponseInputImageContentParam{}
	if im.ImageURL != "" {
		p.ImageURL = param.NewOpt(im.ImageURL)
	}
	if im.FileID != "" {
		p.FileID = param.NewOpt(im.FileID)
	}
	if im.Detail != "" {
		p.Detail = responses.ResponseInputImageContentDetail(string(im.Detail))
	}
	return responses.ResponseFunctionCallOutputItemUnionParam{OfInputImage: &p}
}

// ToolOutputFile is a file content part: one of FileData (base64), FileURL or
// FileID; Filename is optional metadata shown to the model.
type ToolOutputFile struct {
	FileData string
	FileURL  string
	FileID   string
	Filename string
}

func (ToolOutputFile) isToolOutputContent() {}

func (f ToolOutputFile) toContentParam() responses.ResponseFunctionCallOutputItemUnionParam {
	p := responses.ResponseInputFileContentParam{}
	if f.FileData != "" {
		p.FileData = param.NewOpt(f.FileData)
	}
	if f.FileURL != "" {
		p.FileURL = param.NewOpt(f.FileURL)
	}
	if f.FileID != "" {
		p.FileID = param.NewOpt(f.FileID)
	}
	if f.Filename != "" {
		p.Filename = param.NewOpt(f.Filename)
	}
	return responses.ResponseFunctionCallOutputItemUnionParam{OfInputFile: &p}
}

// DataURL builds a base64 data: URL from raw bytes and a MIME type.
func DataURL(mimeType string, data []byte) string {
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// ToolOutputImageFromBytes builds an image content part from raw bytes and a MIME type.
func ToolOutputImageFromBytes(mimeType string, data []byte) ToolOutputImage {
	return ToolOutputImage{ImageURL: DataURL(mimeType, data)}
}

// toolOutputContentItem builds a content-list function_call_output item for a
// ToolOutputContent or non-empty slice; false means use the string path.
func toolOutputContentItem(callID string, output any) (InputItem, bool) {
	var parts []ToolOutputContent
	switch v := output.(type) {
	case ToolOutputContent:
		parts = []ToolOutputContent{v}
	case []ToolOutputContent:
		if len(v) == 0 {
			return InputItem{}, false
		}
		parts = v
	default:
		return InputItem{}, false
	}
	list := make(responses.ResponseFunctionCallOutputItemListParam, len(parts))
	for i, p := range parts {
		list[i] = p.toContentParam()
	}
	return oaiitems.FunctionCallOutput(callID, responses.ResponseInputItemFunctionCallOutputOutputUnionParam{OfResponseFunctionCallOutputItemArray: list}), true
}
