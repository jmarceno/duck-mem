package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

const (
	sniInterface  = "org.kde.StatusNotifierItem"
	menuInterface = "com.canonical.dbusmenu"
	watcherName   = "org.kde.StatusNotifierWatcher"
	sniPath       = dbus.ObjectPath("/StatusNotifierItem")
	menuPath      = dbus.ObjectPath("/Menu")
)

type iconPixmap struct {
	Width, Height int32
	Data          []byte // ARGB32, in network byte order
}

type trayTooltip struct {
	IconName    string
	IconPixmaps []iconPixmap
	Title       string
	Text        string
}

type menuLayout struct {
	ID         int32
	Properties map[string]dbus.Variant
	Children   []dbus.Variant
}

type menuProperties struct {
	ID         int32
	Properties map[string]dbus.Variant
}

type menuEvent struct {
	ID        int32
	EventID   string
	Data      dbus.Variant
	Timestamp uint32
}

type trayApp struct {
	conn  *dbus.Conn
	props *prop.Properties
	icons trayIcons
	mu    sync.Mutex
	state traySnapshot
}

type traySnapshot struct {
	running  bool
	busy     bool
	action   string
	last     syncStatus
	lastSeen bool
	revision uint32
}

func runTray() error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return err
	}
	defer conn.Close()
	name := fmt.Sprintf("org.freedesktop.StatusNotifierItem-%d-1", os.Getpid())
	reply, err := conn.RequestName(name, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		return fmt.Errorf("request SNI bus name: %v (reply %d)", err, reply)
	}
	icons, err := newTrayIcons()
	if err != nil {
		return fmt.Errorf("tray icon: %w", err)
	}
	app := &trayApp{conn: conn, icons: icons}
	app.refresh()
	if err := conn.Export(app, sniPath, sniInterface); err != nil {
		return err
	}
	if err := conn.Export(app, menuPath, menuInterface); err != nil {
		return err
	}
	if err := app.exportProperties(); err != nil {
		return err
	}
	if err := exportTrayIntrospection(conn); err != nil {
		return err
	}
	if err := registerTray(conn, name); err != nil {
		return err
	}
	if err := conn.AddMatchSignal(dbus.WithMatchInterface("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"), dbus.WithMatchArg(0, watcherName)); err != nil {
		return err
	}
	signals := make(chan *dbus.Signal, 8)
	conn.Signal(signals)
	defer conn.RemoveSignal(signals)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			app.refresh()
		case sig := <-signals:
			if sig != nil && sig.Name == "org.freedesktop.DBus.NameOwnerChanged" && len(sig.Body) == 3 {
				if owner, ok := sig.Body[2].(string); ok && owner != "" {
					if err := registerTray(conn, name); err != nil {
						log.Printf("register tray after watcher restart: %v", err)
					}
				}
			}
		}
	}
}

func registerTray(conn *dbus.Conn, name string) error {
	return conn.Object(watcherName, "/StatusNotifierWatcher").
		Call(watcherName+".RegisterStatusNotifierItem", 0, name).Err
}

func (a *trayApp) exportProperties() error {
	s := a.snapshot()
	props, err := prop.Export(a.conn, sniPath, prop.Map{sniInterface: {
		"Category":            {Value: "SystemServices", Emit: prop.EmitConst},
		"Id":                  {Value: "duck-mem", Emit: prop.EmitConst},
		"Title":               {Value: "duck-mem", Emit: prop.EmitConst},
		"Status":              {Value: "Active", Emit: prop.EmitConst},
		"WindowId":            {Value: uint32(0), Emit: prop.EmitConst},
		"IconName":            {Value: "", Emit: prop.EmitConst},
		"IconPixmap":          {Value: a.icons.logo, Emit: prop.EmitTrue},
		"OverlayIconName":     {Value: "", Emit: prop.EmitConst},
		"OverlayIconPixmap":   {Value: a.icons.overlay(s), Emit: prop.EmitTrue},
		"AttentionIconName":   {Value: "", Emit: prop.EmitConst},
		"AttentionIconPixmap": {Value: []iconPixmap{}, Emit: prop.EmitConst},
		"AttentionMovieName":  {Value: "", Emit: prop.EmitConst},
		"ToolTip":             {Value: a.tooltip(s), Emit: prop.EmitTrue},
		"Menu":                {Value: menuPath, Emit: prop.EmitConst},
		"ItemIsMenu":          {Value: true, Emit: prop.EmitConst},
	}})
	if err != nil {
		return err
	}
	a.props = props
	_, err = prop.Export(a.conn, menuPath, prop.Map{menuInterface: {
		"Version":       {Value: uint32(3), Emit: prop.EmitConst},
		"TextDirection": {Value: "ltr", Emit: prop.EmitConst},
		"Status":        {Value: "normal", Emit: prop.EmitConst},
		"IconThemePath": {Value: []string{}, Emit: prop.EmitConst},
	}})
	return err
}

func (a *trayApp) snapshot() traySnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

func (a *trayApp) refresh() bool {
	running := exec.Command("systemctl", "--user", "is-active", "--quiet", serviceName).Run() == nil
	last, err := readSyncStatus()
	seen := err == nil && !last.CompletedAt.IsZero()
	a.mu.Lock()
	changed := a.state.running != running || a.state.lastSeen != seen || a.state.last != last
	a.state.running, a.state.last, a.state.lastSeen = running, last, seen
	if changed {
		a.state.revision++
	}
	s := a.state
	a.mu.Unlock()
	if changed && a.props != nil {
		a.publish(s)
	}
	return changed
}

func (a *trayApp) publish(s traySnapshot) {
	a.props.SetMust(sniInterface, "IconPixmap", a.icons.logo)
	a.props.SetMust(sniInterface, "OverlayIconPixmap", a.icons.overlay(s))
	a.props.SetMust(sniInterface, "ToolTip", a.tooltip(s))
	_ = a.conn.Emit(sniPath, sniInterface+".NewIcon")
	_ = a.conn.Emit(sniPath, sniInterface+".NewToolTip")
	_ = a.conn.Emit(menuPath, menuInterface+".LayoutUpdated", s.revision, int32(0))
}

// tooltip carries the logo next to the last-sync line; hosts that only render
// the tooltip icon still show something recognisable.
func (a *trayApp) tooltip(s traySnapshot) trayTooltip {
	logo := pickPixmap(a.icons.logo, 24)
	return trayTooltip{"", []iconPixmap{logo}, "duck-mem", a.lastSyncLabel(s)}
}

func (a *trayApp) beginAction(action string) bool {
	a.mu.Lock()
	if a.state.busy {
		a.mu.Unlock()
		return false
	}
	a.state.busy = true
	a.state.action = action
	a.state.revision++
	s := a.state
	a.mu.Unlock()
	if a.props != nil {
		a.publish(s)
	}
	return true
}

func (a *trayApp) endAction() {
	a.mu.Lock()
	a.state.busy = false
	a.state.action = ""
	a.state.revision++
	s := a.state
	a.mu.Unlock()
	if a.props != nil {
		a.publish(s)
	}
	a.refresh()
}

func (a *trayApp) lastSyncLabel(s traySnapshot) string {
	if !s.lastSeen {
		return "Last sync: never"
	}
	label := "Last sync: " + s.last.CompletedAt.Local().Format("2006-01-02 15:04:05")
	if s.last.Skipped > 0 {
		label += fmt.Sprintf(" (%d skipped)", s.last.Skipped)
	}
	return label
}

// StatusNotifierItem methods. The host displays the exported DBusMenu.
func (a *trayApp) ContextMenu(x, y int32) *dbus.Error                 { return nil }
func (a *trayApp) Activate(x, y int32) *dbus.Error                    { return nil }
func (a *trayApp) SecondaryActivate(x, y int32) *dbus.Error           { return nil }
func (a *trayApp) Scroll(delta int32, orientation string) *dbus.Error { return nil }

func menuItem(id int32, label string, enabled bool) menuLayout {
	return menuLayout{ID: id, Properties: map[string]dbus.Variant{
		"label": dbus.MakeVariant(label), "enabled": dbus.MakeVariant(enabled),
	}, Children: []dbus.Variant{}}
}

func (a *trayApp) menuItems(s traySnapshot) []menuLayout {
	status := "Indexer: stopped"
	if s.running {
		status = "Indexer: running"
	}
	if s.busy {
		status = "Indexer: " + s.action
	}
	return []menuLayout{
		menuItem(1, status, false),
		menuItem(2, a.lastSyncLabel(s), false),
		{ID: 3, Properties: map[string]dbus.Variant{"type": dbus.MakeVariant("separator")}, Children: []dbus.Variant{}},
		menuItem(4, "Start daemon", !s.running && !s.busy),
		menuItem(5, "Stop daemon", s.running && !s.busy),
		menuItem(6, "Full re-index", !s.busy),
	}
}

func selectProperties(props map[string]dbus.Variant, names []string) map[string]dbus.Variant {
	if len(names) == 0 {
		return props
	}
	out := make(map[string]dbus.Variant)
	for _, name := range names {
		if v, ok := props[name]; ok {
			out[name] = v
		}
	}
	return out
}

func (a *trayApp) GetLayout(parentID, depth int32, names []string) (uint32, menuLayout, *dbus.Error) {
	a.refresh()
	s := a.snapshot()
	items := a.menuItems(s)
	if parentID != 0 {
		for _, item := range items {
			if item.ID == parentID {
				item.Properties = selectProperties(item.Properties, names)
				return s.revision, item, nil
			}
		}
		return 0, menuLayout{}, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", []any{"unknown menu item"})
	}
	root := menuLayout{ID: 0, Properties: map[string]dbus.Variant{}, Children: []dbus.Variant{}}
	if depth != 0 {
		for _, item := range items {
			item.Properties = selectProperties(item.Properties, names)
			root.Children = append(root.Children, dbus.MakeVariant(item))
		}
	}
	return s.revision, root, nil
}

func (a *trayApp) GetGroupProperties(ids []int32, names []string) ([]menuProperties, *dbus.Error) {
	a.refresh()
	items := a.menuItems(a.snapshot())
	requested := make(map[int32]bool)
	for _, id := range ids {
		requested[id] = true
	}
	out := make([]menuProperties, 0, len(items))
	for _, item := range items {
		if len(ids) == 0 || requested[item.ID] {
			out = append(out, menuProperties{item.ID, selectProperties(item.Properties, names)})
		}
	}
	return out, nil
}

func (a *trayApp) GetProperty(id int32, name string) (dbus.Variant, *dbus.Error) {
	a.refresh()
	for _, item := range a.menuItems(a.snapshot()) {
		if item.ID == id {
			if v, ok := item.Properties[name]; ok {
				return v, nil
			}
		}
	}
	return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", []any{"unknown menu property"})
}

func (a *trayApp) Event(id int32, eventID string, data dbus.Variant, timestamp uint32) *dbus.Error {
	if eventID != "clicked" {
		return nil
	}
	a.refresh()
	s := a.snapshot()
	switch id {
	case 4:
		if !s.running && a.beginAction("starting") {
			go a.runSystemctl("start")
		}
	case 5:
		if s.running && a.beginAction("stopping") {
			go a.runSystemctl("stop")
		}
	case 6:
		if a.beginAction("re-indexing") {
			go a.fullReindex(s.running)
		}
	}
	return nil
}

func (a *trayApp) EventGroup(events []menuEvent) ([]int32, *dbus.Error) {
	for _, event := range events {
		_ = a.Event(event.ID, event.EventID, event.Data, event.Timestamp)
	}
	return []int32{}, nil
}

func (a *trayApp) AboutToShow(id int32) (bool, *dbus.Error) {
	return a.refresh(), nil
}

func (a *trayApp) AboutToShowGroup(ids []int32) ([]int32, []int32, *dbus.Error) {
	if a.refresh() {
		return ids, []int32{}, nil
	}
	return []int32{}, []int32{}, nil
}

func (a *trayApp) runSystemctl(action string) {
	defer a.endAction()
	if err := systemctl(action, serviceName); err != nil {
		log.Printf("%s daemon: %v", action, err)
	}
}

func (a *trayApp) fullReindex(wasRunning bool) {
	defer a.endAction()
	if wasRunning {
		if err := systemctl("stop", serviceName); err != nil {
			log.Printf("stop daemon for re-index: %v", err)
			return
		}
		defer func() {
			if err := systemctl("start", serviceName); err != nil {
				log.Printf("restart daemon after re-index: %v", err)
			}
		}()
	}
	self, err := os.Executable()
	if err != nil {
		log.Printf("full re-index: %v", err)
		return
	}
	cmd := exec.Command(self, "index", "--full")
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("full re-index: %v: %s", err, strings.TrimSpace(string(out)))
	} else {
		log.Printf("full re-index complete")
	}
}

func exportTrayIntrospection(conn *dbus.Conn) error {
	sni := introspect.Introspectable(introspect.IntrospectDeclarationString + `<node>
<interface name="org.kde.StatusNotifierItem">
  <method name="Activate"><arg type="i" direction="in"/><arg type="i" direction="in"/></method>
  <method name="SecondaryActivate"><arg type="i" direction="in"/><arg type="i" direction="in"/></method>
  <method name="ContextMenu"><arg type="i" direction="in"/><arg type="i" direction="in"/></method>
  <method name="Scroll"><arg type="i" direction="in"/><arg type="s" direction="in"/></method>
  <property name="Category" type="s" access="read"/><property name="Id" type="s" access="read"/>
  <property name="Title" type="s" access="read"/><property name="Status" type="s" access="read"/>
  <property name="WindowId" type="u" access="read"/><property name="IconName" type="s" access="read"/>
  <property name="IconPixmap" type="a(iiay)" access="read"/><property name="OverlayIconName" type="s" access="read"/>
  <property name="OverlayIconPixmap" type="a(iiay)" access="read"/><property name="AttentionIconName" type="s" access="read"/>
  <property name="AttentionIconPixmap" type="a(iiay)" access="read"/><property name="AttentionMovieName" type="s" access="read"/>
  <property name="ToolTip" type="(sa(iiay)ss)" access="read"/>
  <property name="Menu" type="o" access="read"/><property name="ItemIsMenu" type="b" access="read"/>
  <signal name="NewIcon"/><signal name="NewToolTip"/><signal name="NewStatus"><arg type="s"/></signal>
  <signal name="NewTitle"/><signal name="NewAttentionIcon"/><signal name="NewOverlayIcon"/>
</interface><interface name="org.freedesktop.DBus.Properties">
  <method name="Get"><arg type="s" direction="in"/><arg type="s" direction="in"/><arg type="v" direction="out"/></method>
  <method name="GetAll"><arg type="s" direction="in"/><arg type="a{sv}" direction="out"/></method>
  <method name="Set"><arg type="s" direction="in"/><arg type="s" direction="in"/><arg type="v" direction="in"/></method>
</interface></node>`)
	if err := conn.Export(sni, sniPath, "org.freedesktop.DBus.Introspectable"); err != nil {
		return err
	}
	menu := introspect.Introspectable(introspect.IntrospectDeclarationString + `<node>
<interface name="com.canonical.dbusmenu">
  <method name="GetLayout"><arg type="i" direction="in"/><arg type="i" direction="in"/><arg type="as" direction="in"/><arg type="u" direction="out"/><arg type="(ia{sv}av)" direction="out"/></method>
  <method name="GetGroupProperties"><arg type="ai" direction="in"/><arg type="as" direction="in"/><arg type="a(ia{sv})" direction="out"/></method>
  <method name="GetProperty"><arg type="i" direction="in"/><arg type="s" direction="in"/><arg type="v" direction="out"/></method>
  <method name="Event"><arg type="i" direction="in"/><arg type="s" direction="in"/><arg type="v" direction="in"/><arg type="u" direction="in"/></method>
  <method name="EventGroup"><arg type="a(isvu)" direction="in"/><arg type="ai" direction="out"/></method>
  <method name="AboutToShow"><arg type="i" direction="in"/><arg type="b" direction="out"/></method>
  <method name="AboutToShowGroup"><arg type="ai" direction="in"/><arg type="ai" direction="out"/><arg type="ai" direction="out"/></method>
  <signal name="LayoutUpdated"><arg type="u"/><arg type="i"/></signal>
  <property name="Version" type="u" access="read"/><property name="TextDirection" type="s" access="read"/>
  <property name="Status" type="s" access="read"/><property name="IconThemePath" type="as" access="read"/>
</interface><interface name="org.freedesktop.DBus.Properties">
  <method name="Get"><arg type="s" direction="in"/><arg type="s" direction="in"/><arg type="v" direction="out"/></method>
  <method name="GetAll"><arg type="s" direction="in"/><arg type="a{sv}" direction="out"/></method>
  <method name="Set"><arg type="s" direction="in"/><arg type="s" direction="in"/><arg type="v" direction="in"/></method>
</interface></node>`)
	return conn.Export(menu, menuPath, "org.freedesktop.DBus.Introspectable")
}
