// Do not edit: file generated by reactGen

package markdowneditor

import "github.com/myitcv/gopherjs/react"

func (p *MarkdownEditorDef) SetState(s MarkdownEditorState) {
	p.ComponentDef.SetState(s)
}

func (p *MarkdownEditorDef) State() MarkdownEditorState {
	return p.ComponentDef.State().(MarkdownEditorState)
}

func (p MarkdownEditorState) IsState() {}

func (p *MarkdownEditorDef) GetInitialStateIntf() react.State {
	return p.GetInitialState()
}