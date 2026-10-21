package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

type deviceColorRow struct {
	Kind  string
	Match string
	Color string
}

type deviceColorGroup struct {
	Kind, Title, Help string
	Rows              []deviceColorRow
	Empty             deviceColorRow
}

func populateDeviceColorsSettings(data *settingsData) {
	colors, err := parseDeviceColors([]byte(data.Value))
	if err != nil {
		data.Error = err.Error()
		return
	}
	exact := deviceColorGroup{Kind: "exact", Title: "Exact device names", Help: "Match the full device name. Exact matches take priority over contains rules.", Empty: deviceColorRow{Kind: "exact", Color: "#0078FF"}}
	names := make([]string, 0, len(colors.Exact))
	for name := range colors.Exact {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		exact.Rows = append(exact.Rows, deviceColorRow{Kind: "exact", Match: name, Color: colors.Exact[name]})
	}
	contains := deviceColorGroup{Kind: "contains", Title: "Device name contains", Help: "Match part of a device name. Rules are case-sensitive; the first matching rule wins. Use the arrows to change priority.", Empty: deviceColorRow{Kind: "contains", Color: "#0078FF"}}
	for _, rule := range colors.Contains {
		contains.Rows = append(contains.Rows, deviceColorRow{Kind: "contains", Match: rule.Match, Color: rule.Color})
	}
	data.DeviceColorGroups = []deviceColorGroup{exact, contains}
}

func deviceColorsFormValue(form url.Values) (string, error) {
	colors := deviceColorSet{Exact: make(map[string]string), Contains: []deviceContainsRule{}}
	for _, kind := range []string{"exact", "contains"} {
		matches, values := form[kind+"_match"], form[kind+"_color"]
		if len(matches) != len(values) {
			return "", fmt.Errorf("each device rule needs a name and color")
		}
		for i, match := range matches {
			match = strings.TrimSpace(match)
			if match == "" {
				return "", fmt.Errorf("enter a device name or remove the empty rule")
			}
			color, ok := normalizeHexColor(values[i])
			if !ok {
				return "", fmt.Errorf("color for %q must use #RRGGBB format", match)
			}
			if kind == "exact" {
				if !validName(match) {
					return "", fmt.Errorf("invalid device name %q", match)
				}
				if _, exists := colors.Exact[match]; exists {
					return "", fmt.Errorf("device %q has more than one exact color rule", match)
				}
				colors.Exact[match] = color
			} else {
				colors.Contains = append(colors.Contains, deviceContainsRule{Match: match, Color: color})
			}
		}
	}
	encoded, err := json.Marshal(colors)
	return string(encoded), err
}

const deviceColorsSettingsTemplate = `
{{define "device-color-row"}}
<div class="device-color-row">
  <input type="text" name="{{.Kind}}_match" value="{{.Match}}" placeholder="{{if eq .Kind "exact"}}Device name{{else}}Name contains…{{end}}" aria-label="{{if eq .Kind "exact"}}Exact device name{{else}}Device name contains{{end}}" required>
  <input type="color" value="{{.Color}}" data-color-picker aria-label="Choose device color">
  <input type="text" name="{{.Kind}}_color" value="{{.Color}}" data-color-hex aria-label="Device color hex code" pattern="#[0-9A-Fa-f]{6}" maxlength="7" placeholder="#RRGGBB" required>
  <div class="device-color-buttons">
    {{if eq .Kind "contains"}}<button type="button" data-move="up" aria-label="Move rule up" title="Move up">↑</button><button type="button" data-move="down" aria-label="Move rule down" title="Move down">↓</button>{{end}}
    <button type="button" data-remove-color aria-label="Remove color rule">Remove</button>
  </div>
</div>
{{end}}
{{define "device-colors"}}
<style>
  .device-color-group { background: var(--panel); border: 1px solid var(--line); border-radius: .65rem; margin: 1rem 0; padding: 1rem; }
  .device-color-group h3 { margin: 0; }
  .device-color-group p { margin: .4rem 0 .8rem; }
  .device-color-row { align-items: center; border-top: 1px solid var(--line); display: grid; gap: .65rem; grid-template-columns: minmax(8rem, 1fr) 2.75rem 7rem auto; padding: .8rem 0; }
  .device-color-row input { background: var(--input-bg); border: 1px solid var(--line); border-radius: .45rem; color: var(--ink); font: inherit; min-width: 0; padding: .5rem; width: 100%; }
  .device-color-row input[type=color] { cursor: pointer; height: 2.5rem; padding: .2rem; }
  .device-color-buttons { display: flex; gap: .4rem; }
  .device-color-buttons button { background: var(--panel-soft); border: 1px solid var(--line); color: var(--ink); margin: 0; padding: .45rem .6rem; }
  @media(max-width: 700px) { .device-color-row { grid-template-columns: minmax(0, 1fr) 2.75rem 7rem; } .device-color-buttons { grid-column: 1 / -1; justify-content: flex-end; } }
</style>
<form method="post" action="/settings" data-settings-form id="device-colors-form">
  <input type="hidden" name="section" value="device-colors">
  <input type="hidden" name="csrf" value="{{.CSRF}}">
  {{range .DeviceColorGroups}}
  <section class="device-color-group">
    <h3>{{.Title}}</h3>
    <p class="muted">{{.Help}}</p>
    <div data-color-rows>{{range .Rows}}{{template "device-color-row" .}}{{end}}</div>
    <p class="muted" data-empty-colors {{if .Rows}}hidden{{end}}>No rules added.</p>
    <template data-color-template>{{template "device-color-row" .Empty}}</template>
    <button type="button" data-add-color>Add {{if eq .Kind "exact"}}exact{{else}}contains{{end}} rule</button>
  </section>
  {{end}}
  <div class="error" data-form-error hidden></div>
  <div class="settings-actions">
    <button type="submit" {{if .Error}}disabled{{end}}>Save Device Colors</button>
    <button class="reset-button" type="submit" name="action" value="reset" formnovalidate>Reset to Defaults</button>
  </div>
</form>
<script>
(() => {
  const form = document.getElementById("device-colors-form");
  form.addEventListener("click", event => {
    const button = event.target.closest("button");
    if (!button) return;
    const group = button.closest(".device-color-group");
    if (!group) return;
    const rows = group.querySelector("[data-color-rows]");
    if (button.hasAttribute("data-add-color")) {
      const row = group.querySelector("[data-color-template]").content.firstElementChild.cloneNode(true);
      rows.append(row);
      row.querySelector("input").focus();
    } else if (button.hasAttribute("data-remove-color")) {
      button.closest(".device-color-row").remove();
      group.querySelector("[data-add-color]").focus();
    } else if (button.dataset.move) {
      const row = button.closest(".device-color-row");
      if (button.dataset.move === "up") row.previousElementSibling?.before(row);
      else row.nextElementSibling?.after(row);
    }
    group.querySelector("[data-empty-colors]").hidden = rows.children.length > 0;
  });
  form.addEventListener("input", event => {
    const row = event.target.closest(".device-color-row");
    if (!row) return;
    const picker = row.querySelector("[data-color-picker]");
    const hex = row.querySelector("[data-color-hex]");
    if (event.target === picker) hex.value = picker.value.toUpperCase();
    else if (event.target === hex && /^#[0-9a-f]{6}$/i.test(hex.value)) picker.value = hex.value;
  });
})();
</script>
{{end}}
`
