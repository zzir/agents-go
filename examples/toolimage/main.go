// Command toolimage shows a function tool returning native image input via
// agents.ToolOutputImage, so the model can "see" what the tool produced rather
// than receiving a text description. The tool generates a solid-color PNG in
// memory; the (vision-capable) model is asked what color it is.
package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"

	agents "github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
)

type swatchArgs struct {
	Color string `json:"color" jsonschema:"a CSS-ish color name to render: red, green or blue"`
}

func renderSwatch(_ context.Context, _ *agents.ToolContext, args swatchArgs) (agents.ToolOutputImage, error) {
	fill := map[string]color.RGBA{
		"red":   {R: 220, A: 255},
		"green": {G: 200, A: 255},
		"blue":  {B: 230, A: 255},
	}[args.Color]
	if fill.A == 0 {
		fill = color.RGBA{R: 128, G: 128, B: 128, A: 255}
	}
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := range 32 {
		for x := range 32 {
			img.Set(x, y, fill)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return agents.ToolOutputImage{}, err
	}
	return agents.ToolOutputImageFromBytes("image/png", buf.Bytes()), nil
}

func main() {
	swatch := agents.NewFunctionTool("render_swatch",
		"Render a 32x32 solid-color PNG swatch and return it as an image.", renderSwatch)

	agent := &agents.Agent{
		Name:         "vision-bot",
		Instructions: agents.StaticInstructions("Use the render_swatch tool, then describe the image you get back."),
		Model:        "gpt-4o",
		Tools:        []agents.Tool{swatch},
	}

	res, err := agents.RunSync(context.Background(), agent,
		"Render a blue swatch and tell me what color you see.",
		agents.RunOptions{ModelProvider: openai.NewProvider()})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.FinalOutputString())
}
