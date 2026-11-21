package localviewer

import (
	"os/exec"
	"strings"
	"testing"
)

func TestFileCommitCardUsesTextOnly(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable")
	}
	data, err := assets.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	start := strings.Index(source, "async function renderCommit(")
	end := strings.Index(source, "  function closeStream()")
	if start < 0 || end <= start {
		t.Fatal("commit renderer missing")
	}
	script := `const assert=require("node:assert/strict");
const elements = new Map();
const $ = id => {
  if (!elements.has(id)) elements.set(id, {textContent:"", hidden:false, set innerHTML(v){throw Error("markup sink");}});
  return elements.get(id);
};
let commitShown=null;
const clearShot=()=>{};
` + source[start:end] + `
(async()=>{
  await renderCommit({commit:{id:"fake", action:"Submit", domain:"example.test", reason:"form", files:[{name:"<img onerror=alert(1)>.txt",size:13}], has_shot:false}});
  assert.match($("commit-reason").textContent, /Attached files: <img onerror=alert\(1\)>.txt \(13 bytes\)/);
  await renderCommit({commit:{id:"next", action:"Submit", domain:"example.test", reason:"form", has_shot:false}});
  assert.doesNotMatch($("commit-reason").textContent, /Attached files/);
})().catch(e=>{console.error(e);process.exitCode=1});`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("file renderer: %v %s", err, out)
	}
}
