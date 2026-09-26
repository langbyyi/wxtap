package ipc

import (
	"strings"
	"testing"
)

func TestVConsoleExpressionEnablesDebug(t *testing.T) {
	expression := VConsoleExpression(true)
	if !strings.Contains(expression, "enableDebug:true") {
		t.Fatalf("enable not set: %s", expression)
	}
	if !strings.Contains(expression, "nav.wxFrame.wx.setEnableDebug") {
		t.Fatalf("setEnableDebug call missing: %s", expression)
	}
	if !strings.Contains(expression, "JSON.stringify({err:'no wxFrame'})") {
		t.Fatalf("no-wxFrame guard missing: %s", expression)
	}
}

func TestVConsoleExpressionDisablesDebug(t *testing.T) {
	expression := VConsoleExpression(false)
	if !strings.Contains(expression, "enableDebug:false") {
		t.Fatalf("disable not set: %s", expression)
	}
}
