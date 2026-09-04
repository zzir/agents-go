package agents

import (
	"encoding/base64"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
)

// ToolOutputContent is one content part of a structured tool result fed back to
// the model as function_call_output content. A function tool returns a value
// implementing this interface — or a []ToolOutputContent for several parts — to
// hand the model native text, image or file input instead of a plain string or
// JSON result.
//
// The interface is sealed: ToolOutputText, ToolOutputImage and ToolOutputFile
// are its only implementations, mirroring the Responses API's input_text,
// input_image and input_file content parts.
//
// A tool that returns an ordinary value (string, struct, …) still has it
// stringified as before; only values implementing this interface take the
// multimodal content path.
type ToolOutputContent interface {
	isToolOutputContent()
	toContentParam() responses.ResponseFunctionCallOutputItemUnionParam
}

// ToolOutputText is a plain-text content part. It is equivalent to returning the
// string directly, but can be combined with images/files in a []ToolOutputContent.
type ToolOutputText struct {
	Text string
}

func (ToolOutputText) isToolOutputContent() {}

func (t ToolOutputText) toContentParam() responses.ResponseFunctionCallOutputItemUnionParam {
	return responses.ResponseFunctionCallOutputItemParamOfInputText(t.Text)
}

// ToolOutputImageDetail is the requested fidelity of a ToolOutputImage. Use the
// DetailLow, DetailHigh, DetailAuto or DetailOriginal constants.
type ToolOutputImageDetail string

// The predefined image-detail levels.
const (
	DetailLow      ToolOutputImageDetail = "low"
	DetailHigh     ToolOutputImageDetail = "high"
	DetailAuto     ToolOutputImageDetail = "auto"
	DetailOriginal ToolOutputImageDetail = "original"
)

// ToolOutputImage is an image content part handed to the model as native image
// input. Set exactly one of ImageURL (a fully-qualified URL, or a data: URL
// carrying base64 image data) or FileID (an already-uploaded OpenAI file).
// Detail is optional: DetailLow, DetailHigh, DetailAuto or DetailOriginal.
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

// ToolOutputFile is a file content part (e.g. a PDF) handed to the model as
// native file input. Set one of FileData (base64-encoded bytes), FileURL or
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

// DataURL builds a base64 data: URL from raw bytes and a MIME type, suitable for
// ToolOutputImage.ImageURL.
func DataURL(mimeType string, data []byte) string {
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// ToolOutputImageFromBytes builds an image content part from raw image bytes and
// a MIME type, encoding them as a base64 data URL.
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
	return responses.ResponseInputItemParamOfFunctionCallOutput(callID, list), true
}
