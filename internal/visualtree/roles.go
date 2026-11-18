package visualtree

// AT-SPI roles (AtspiRole) and states (AtspiStateType), as the reader
// sends them. Only BrowserPier's own words for them are ever printed: an
// unknown role is "generic", never what the bus said.

const (
	roleAlert            = 2
	roleCheckBox         = 7
	roleCheckMenuItem    = 8
	roleComboBox         = 11
	roleDialog           = 16
	roleImage            = 27
	roleInternalFrame    = 28
	roleLabel            = 29
	roleList             = 31
	roleListItem         = 32
	roleMenu             = 33
	roleMenuBar          = 34
	roleMenuItem         = 35
	rolePageTab          = 37
	rolePageTabList      = 38
	rolePasswordText     = 40
	rolePopupMenu        = 41
	roleProgressBar      = 42
	rolePushButton       = 43
	roleRadioButton      = 44
	roleRadioMenuItem    = 45
	roleSlider           = 51
	roleSpinButton       = 52
	roleTableCell        = 56
	roleText             = 61
	roleToggleButton     = 62
	roleTree             = 65
	roleParagraph        = 73
	roleEntry            = 79
	roleHeading          = 83
	roleSection          = 85
	roleForm             = 87
	roleLink             = 88
	roleTreeItem         = 91
	roleDocumentWeb      = 95
	roleListBox          = 98
	roleLandmark         = 110
	roleStatic           = 116
	rolePushButtonMenu   = 129
	roleDescriptionTerm  = 122
	roleDescriptionValue = 123
)

// controlRoles are the roles shown as controls, with a ref, by the word
// the API-mode page tools use for the same control.
var controlRoles = map[uint32]string{
	rolePushButton:     "button",
	rolePushButtonMenu: "button",
	roleToggleButton:   "button",
	roleLink:           "link",
	roleEntry:          "textbox",
	roleText:           "textbox",
	rolePasswordText:   "textbox",
	roleCheckBox:       "checkbox",
	roleRadioButton:    "radio",
	roleComboBox:       "combobox",
	roleListBox:        "listbox",
	roleMenuItem:       "menuitem",
	roleCheckMenuItem:  "menuitemcheckbox",
	roleRadioMenuItem:  "menuitemradio",
	roleSlider:         "slider",
	roleSpinButton:     "spinbutton",
	rolePageTab:        "tab",
	roleTreeItem:       "treeitem",
}

// roleWord is a role as BrowserPier prints it.
func roleWord(role uint32) string {
	if w, ok := controlRoles[role]; ok {
		return w
	}
	switch role {
	case roleDialog:
		return "dialog"
	case roleAlert:
		return "alert"
	case roleImage:
		return "image"
	case roleHeading:
		return "heading"
	case roleForm:
		return "form"
	case roleList:
		return "list"
	case roleMenu, rolePopupMenu:
		return "menu"
	case roleMenuBar:
		return "menubar"
	case roleTree:
		return "tree"
	case rolePageTabList:
		return "tablist"
	case roleProgressBar:
		return "progressbar"
	}
	return "generic"
}

// AT-SPI states, as bits.
const (
	stateArmed          = 2
	stateBusy           = 3
	stateChecked        = 4
	stateEditable       = 7
	stateEnabled        = 8
	stateExpandable     = 9
	stateExpanded       = 10
	stateFocused        = 12
	stateModal          = 16
	statePressed        = 20
	stateSelected       = 23
	stateSensitive      = 24
	stateShowing        = 25
	stateVisible        = 30
	stateIndeterminate  = 32
	stateRequired       = 33
	stateInvalidEntry   = 36
	stateCheckable      = 41
	stateReadOnly       = 43
	stateDefunct        = 6
	stateHasPopup       = 42
	stateMultiLine      = 17
	stateSelectableText = 38
)

func has(states uint64, bit int) bool { return states&(1<<bit) != 0 }

// stateWords are a control's states as BrowserPier prints them, in a
// fixed order.
func stateWords(role uint32, states uint64) []string {
	var out []string
	switch {
	case has(states, stateIndeterminate) && (role == roleCheckBox || role == roleCheckMenuItem):
		out = append(out, "mixed")
	case has(states, stateChecked):
		out = append(out, "checked")
	}
	if has(states, statePressed) {
		out = append(out, "pressed")
	}
	if has(states, stateSelected) && role != roleMenuItem && role != rolePageTab {
		out = append(out, "selected")
	}
	if role == rolePageTab && has(states, stateSelected) {
		out = append(out, "selected")
	}
	if has(states, stateExpandable) {
		if has(states, stateExpanded) {
			out = append(out, "expanded")
		} else {
			out = append(out, "collapsed")
		}
	}
	if !has(states, stateEnabled) && !has(states, stateSensitive) {
		out = append(out, "disabled")
	}
	if has(states, stateReadOnly) && (role == roleEntry || role == roleText) {
		out = append(out, "read-only")
	}
	if has(states, stateRequired) {
		out = append(out, "required")
	}
	if has(states, stateInvalidEntry) {
		out = append(out, "invalid")
	}
	if has(states, stateFocused) {
		out = append(out, "focused")
	}
	return out
}
