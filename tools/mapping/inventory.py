#!/usr/bin/env python3
"""
inventory.py - Stock controller mapping inventory extractor
Reads the Engine OS 5.3.0 controller assignment files and qrc modules and writes one
JSON file per board (stable control ids "<module>:<note>"), which the console embeds.
Standard library only; runs with `python3 -I`.
"""

import argparse
import glob
import json
import os
import re
import sys

# Action enum names defined in Action.qml
ACTION_NAMES = {
    0: "None",
    1: "BeatJump",
    2: "ToggleControlCenter",
    3: "ToggleMotor",
    4: "ToggleMotorSpeed",
    5: "ToggleUserProfile",
    6: "GridCueEdit",
    7: "GridEdit",
    8: "InstantDouble",
    9: "KeySync",
    10: "Quantize",
    11: "SetCuePoint",
    12: "Backtrack",
    13: "SwapDeckInfos",
    14: "SwitchMainViewLayout",
    15: "Scratch",
    16: "ToggleMicTalkover",
    17: "ShowSoundSwitchView",
    18: "IncreaseBeatJumpSize",
    19: "SlipMode",
    20: "KeyReset",
    21: "ToggleChannelStripMode",
}

def resolve_action(val):
    if not val:
        return "None"
    val = str(val).strip()
    if val.startswith("Action."):
        return val[len("Action."):]
    try:
        idx = int(val, 0)
        return ACTION_NAMES.get(idx, f"Action({idx})")
    except ValueError:
        return val

def parse_num(val):
    if val is None:
        return None
    val = str(val).strip()
    try:
        return int(val, 0)
    except ValueError:
        return val

def strip_comments(text):
    text = re.sub(r"/\*.*?\*/", "", text, flags=re.S)
    lines = []
    for l in text.splitlines():
        l_clean = re.sub(r"//.*$", "", l)
        lines.append(l_clean)
    return "\n".join(lines)

def parse_qml_blocks(text):
    text = strip_comments(text)
    pos = 0
    length = len(text)

    def skip_ws(p):
        while p < length and text[p].isspace():
            p += 1
        return p

    def parse_object(p):
        p = skip_ws(p)
        m = re.match(r"([A-Za-z0-9_]+)\s*\{", text[p:])
        if not m:
            return None, p
        obj_type = m.group(1)
        p += m.end()

        properties = {}
        children = []

        while p < length:
            p = skip_ws(p)
            if p >= length or text[p] == "}":
                p += 1
                break

            m_child = re.match(r"([A-Z][A-Za-z0-9_]*)\s*\{", text[p:])
            if m_child:
                child_obj, p = parse_object(p)
                if child_obj:
                    children.append(child_obj)
                continue

            m_prop = re.match(r"(?:(?:readonly\s+)?property\s+[A-Za-z0-9_<>\s]+\s+)?([a-zA-Z0-9_]+)\s*:\s*", text[p:])
            if m_prop:
                prop_name = m_prop.group(1)
                p += m_prop.end()
                p = skip_ws(p)

                m_val_obj = re.match(r"([A-Z][A-Za-z0-9_]*)\s*\{", text[p:])
                if m_val_obj:
                    val_obj, p = parse_object(p)
                    properties[prop_name] = val_obj
                    continue

                if text[p] == "[":
                    start_b = p
                    depth = 1
                    p += 1
                    while p < length and depth > 0:
                        if text[p] == "[": depth += 1
                        elif text[p] == "]": depth -= 1
                        p += 1
                    properties[prop_name] = text[start_b:p].strip()
                    continue

                if text[p] == "{":
                    start_b = p
                    depth = 1
                    p += 1
                    while p < length and depth > 0:
                        if text[p] == "{": depth += 1
                        elif text[p] == "}": depth -= 1
                        p += 1
                    properties[prop_name] = text[start_b:p].strip()
                    continue

                m_val = re.match(r"([^;\n\}]+)[;\n]?", text[p:])
                if m_val:
                    properties[prop_name] = m_val.group(1).strip()
                    p += len(m_val.group(0))
                else:
                    p += 1
                continue

            nl = text.find("\n", p)
            if nl == -1:
                p = length
            else:
                p = nl + 1

        return {"type": obj_type, "props": properties, "children": children}, p

    blocks = []
    p = 0
    while p < length:
        p = skip_ws(p)
        if p >= length:
            break
        obj, p = parse_object(p)
        if obj:
            blocks.append(obj)
        else:
            p += 1

    return blocks

def extract_list_elements(obj):
    elements = []
    if not isinstance(obj, dict):
        return elements
    for c in obj.get("children", []):
        if c.get("type") == "ListElement":
            elements.append(c.get("props", {}))
    return elements

def build_board_inventory(board_name, board_dir, qrc_dir):
    qml_files = sorted(glob.glob(f"{board_dir}/*Assignments.qml"))
    controls = []

    global_midi_channel = 0
    deck_midi_channel = 0

    for qml_path in qml_files:
        filename = os.path.basename(qml_path)
        with open(qml_path) as f:
            content = f.read()

        parsed = parse_qml_blocks(content)
        if not parsed:
            continue

        root = parsed[0]
        # find globalConfig
        for c in root.get("children", []):
            if c.get("type") == "GlobalAssignmentConfig":
                if "midiChannel" in c.get("props", {}):
                    global_midi_channel = parse_num(c["props"]["midiChannel"]) or 0

        # Process nodes
        def process_node(node, scope_info):
            nonlocal controls
            ntype = node.get("type")
            props = node.get("props", {})
            children = node.get("children", [])

            # Scoping: DeckAssignmentConfig, MixerChannelAssignmentConfig
            current_scope = dict(scope_info)
            if ntype == "DeckAssignmentConfig":
                if "midiChannel" in props:
                    val = props["midiChannel"]
                    if val != "globalConfig.midiChannel":
                        current_scope["channel"] = parse_num(val)
                if "name" in props:
                    current_scope["deck"] = props["name"].strip('"')
            elif ntype == "MixerChannelAssignmentConfig":
                if "midiChannel" in props:
                    current_scope["channel"] = parse_num(props["midiChannel"])
                if "name" in props:
                    current_scope["mixer_channel"] = props["name"].strip('"')

            ch = current_scope.get("channel", global_midi_channel)
            prefix = ""
            if "deck" in current_scope and current_scope["deck"]:
                prefix = f"Deck {current_scope['deck']} "
            elif "mixer_channel" in current_scope and current_scope["mixer_channel"]:
                prefix = f"Channel {current_scope['mixer_channel']} "

            # Handle Repeater
            if ntype == "Repeater":
                model_obj = props.get("model")
                elems = []
                if isinstance(model_obj, dict):
                    elems = extract_list_elements(model_obj)
                elif isinstance(model_obj, str) and model_obj.isdigit():
                    elems = [{"index": i} for i in range(int(model_obj))]

                for elem in elems:
                    elem_scope = dict(current_scope)
                    if "deckName" in elem:
                        elem_scope["deck"] = elem["deckName"].strip('"')
                    if "deckMidiChannel" in elem:
                        elem_scope["channel"] = parse_num(elem["deckMidiChannel"])
                    if "mixerChannelName" in elem:
                        elem_scope["mixer_channel"] = elem["mixerChannelName"].strip('"')
                    if "mixerChannelMidiChannel" in elem:
                        elem_scope["channel"] = parse_num(elem["mixerChannelMidiChannel"])

                    for item in children:
                        # Item container inside Repeater
                        if item.get("type") == "Item":
                            # Check if item itself has special bindings
                            # Also check if Repeater element defines loadNote or similar
                            for sub in item.get("children", []):
                                # check if sub is Load and elem has loadNote
                                if sub.get("type") == "Load" and "loadNote" in elem:
                                    sub_props = dict(sub.get("props", {}))
                                    sub_props["note"] = elem["loadNote"]
                                    process_node({"type": sub["type"], "props": sub_props, "children": sub.get("children", [])}, elem_scope)
                                else:
                                    process_node(sub, elem_scope)
                        else:
                            process_node(item, elem_scope)
                return

            # Match module types
            if ntype == "PlayCue":
                p_note = parse_num(props.get("playNote"))
                c_note = parse_num(props.get("cueNote"))
                cue_shift = resolve_action(props.get("cueShiftAction", "Action.None"))
                if p_note is not None:
                    controls.append({
                        "control": f"{prefix}Play/Pause",
                        "module": "PlayCue",
                        "type": "Note",
                        "channel": ch,
                        "number": p_note,
                        "press": "Play/Pause toggle (PlayPause)",
                        "shift_press": "Play Stutter / Return to start",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": f"cueShiftAction={cue_shift}",
                        "source_file": filename
                    })
                if c_note is not None:
                    controls.append({
                        "control": f"{prefix}Cue",
                        "module": "PlayCue",
                        "type": "Note",
                        "channel": ch,
                        "number": c_note,
                        "press": "Cue (Cue/Stop; momentary play on hold)",
                        "shift_press": f"Cue Shift: {cue_shift} (e.g. SetInitialCuePoint or Backtrack)",
                        "long_press": "Cue hold momentary playback",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": f"cueShiftAction={cue_shift}",
                        "source_file": filename
                    })

            elif ntype == "BeatJump":
                prev_n = parse_num(props.get("prevNote"))
                next_n = parse_num(props.get("nextNote"))
                turn_cc = parse_num(props.get("turnCC"))
                push_n = parse_num(props.get("pushNote"))
                if prev_n is not None and prev_n != -1:
                    controls.append({
                        "control": f"{prefix}Beat Jump Backward",
                        "module": "BeatJump",
                        "type": "Note",
                        "channel": ch,
                        "number": prev_n,
                        "press": "BeatJump/JumpBackward",
                        "shift_press": "BeatJump/BeatJumpHalve (Halve jump size)",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })
                if next_n is not None and next_n != -1:
                    controls.append({
                        "control": f"{prefix}Beat Jump Forward",
                        "module": "BeatJump",
                        "type": "Note",
                        "channel": ch,
                        "number": next_n,
                        "press": "BeatJump/JumpForward",
                        "shift_press": "BeatJump/BeatJumpDouble (Double jump size)",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })
                if turn_cc is not None and turn_cc != -1:
                    controls.append({
                        "control": f"{prefix}Beat Jump Encoder",
                        "module": "BeatJump",
                        "type": "CC",
                        "channel": ch,
                        "number": turn_cc,
                        "press": "None",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "BeatJump/JumpBackward / JumpForward",
                        "shift_turn": "BeatJump size select",
                        "config": "",
                        "source_file": filename
                    })
                if push_n is not None and push_n != -1:
                    controls.append({
                        "control": f"{prefix}Beat Jump Push",
                        "module": "BeatJump",
                        "type": "Note",
                        "channel": ch,
                        "number": push_n,
                        "press": "Execute Beat Jump",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "TrackSkip":
                prev_n = parse_num(props.get("prevNote"))
                next_n = parse_num(props.get("nextNote"))
                if prev_n is not None:
                    controls.append({
                        "control": f"{prefix}Track Skip Prev",
                        "module": "TrackSkip",
                        "type": "Note",
                        "channel": ch,
                        "number": prev_n,
                        "press": "PreviousTrack",
                        "shift_press": "SeekBackward / Track restart",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })
                if next_n is not None:
                    controls.append({
                        "control": f"{prefix}Track Skip Next",
                        "module": "TrackSkip",
                        "type": "Note",
                        "channel": ch,
                        "number": next_n,
                        "press": "NextTrack",
                        "shift_press": "SeekForward",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "Censor":
                n = parse_num(props.get("note"))
                if n is not None:
                    controls.append({
                        "control": f"{prefix}Censor / Reverse",
                        "module": "Censor",
                        "type": "Note",
                        "channel": ch,
                        "number": n,
                        "press": "Censor / Bleep (SilentCue if censorIsSilentCue)",
                        "shift_press": "Reverse (Track/Direction)",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "ManualLoop":
                in_n = parse_num(props.get("inNote"))
                out_n = parse_num(props.get("outNote"))
                if in_n is not None:
                    controls.append({
                        "control": f"{prefix}Manual Loop In",
                        "module": "ManualLoop",
                        "type": "Note",
                        "channel": ch,
                        "number": in_n,
                        "press": "Set Loop In point",
                        "shift_press": "Move Loop In position (edit mode)",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })
                if out_n is not None:
                    controls.append({
                        "control": f"{prefix}Manual Loop Out",
                        "module": "ManualLoop",
                        "type": "Note",
                        "channel": ch,
                        "number": out_n,
                        "press": "Set Loop Out point",
                        "shift_press": "Move Loop Out position (edit mode)",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "AutoLoop":
                push_n = parse_num(props.get("pushNote"))
                turn_cc = parse_num(props.get("turnCC"))
                loop_inact = resolve_action(props.get("loopInactiveShiftTurnAction", "Action.None"))
                shift_push = resolve_action(props.get("shiftPushAction", "Action.None"))
                if push_n is not None:
                    controls.append({
                        "control": f"{prefix}Auto Loop Push",
                        "module": "AutoLoop",
                        "type": "Note",
                        "channel": ch,
                        "number": push_n,
                        "press": "AutoLoop On/Off toggle (LoopEnableState)",
                        "shift_press": f"Shift Push: {shift_push}",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": f"shiftPushAction={shift_push}, loopInactiveShiftTurnAction={loop_inact}",
                        "source_file": filename
                    })
                if turn_cc is not None:
                    controls.append({
                        "control": f"{prefix}Auto Loop Knob Turn",
                        "module": "AutoLoop",
                        "type": "CC",
                        "channel": ch,
                        "number": turn_cc,
                        "press": "None",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "Halve / Double Loop Size (LoopHalveDouble)",
                        "shift_turn": f"Shift loop position, or BeatJump if loop inactive ({loop_inact})",
                        "config": f"loopInactiveShiftTurnAction={loop_inact}",
                        "source_file": filename
                    })

            elif ntype == "Vinyl":
                n = parse_num(props.get("note"))
                if n is not None:
                    controls.append({
                        "control": f"{prefix}Vinyl Mode",
                        "module": "Vinyl",
                        "type": "Note",
                        "channel": ch,
                        "number": n,
                        "press": "Toggle Vinyl (Scratch) mode",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "Sync":
                s_note = parse_num(props.get("syncNote"))
                m_note = parse_num(props.get("masterNote"))
                sync_hold = resolve_action(props.get("syncHoldAction", "Action.InstantDouble"))
                if s_note is not None:
                    controls.append({
                        "control": f"{prefix}Sync",
                        "module": "Sync",
                        "type": "Note",
                        "channel": ch,
                        "number": s_note,
                        "press": "Sync Toggle (Tempo/Beat Sync)",
                        "shift_press": "Unsync (turn off sync)",
                        "long_press": f"Hold: {sync_hold} (2000 ms)",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": f"syncHoldAction={sync_hold}",
                        "source_file": filename
                    })
                if m_note is not None:
                    controls.append({
                        "control": f"{prefix}Master",
                        "module": "Sync",
                        "type": "Note",
                        "channel": ch,
                        "number": m_note,
                        "press": "Set Deck as Sync Master (Master)",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "KeyLock":
                n = parse_num(props.get("note"))
                s_action = resolve_action(props.get("shiftAction", "Action.KeyReset"))
                h_action = resolve_action(props.get("holdAction", "Action.None"))
                if n is not None:
                    controls.append({
                        "control": f"{prefix}Key Lock",
                        "module": "KeyLock",
                        "type": "Note",
                        "channel": ch,
                        "number": n,
                        "press": "Key Lock Toggle (KeyLock)",
                        "shift_press": f"Shift: {s_action} (KeyReset)",
                        "long_press": f"Hold: {h_action}",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": f"shiftAction={s_action}, holdAction={h_action}",
                        "source_file": filename
                    })

            elif ntype == "Slip":
                n = parse_num(props.get("note"))
                s_action = resolve_action(props.get("shiftAction", "Action.None"))
                h_action = resolve_action(props.get("holdAction", "Action.None"))
                if n is not None:
                    controls.append({
                        "control": f"{prefix}Slip",
                        "module": "Slip",
                        "type": "Note",
                        "channel": ch,
                        "number": n,
                        "press": "Slip Mode Toggle (SlipMode)",
                        "shift_press": f"Shift: {s_action} (e.g. ToggleMotor)",
                        "long_press": f"Hold: {h_action}",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": f"shiftAction={s_action}, holdAction={h_action}",
                        "source_file": filename
                    })

            elif ntype == "PitchBend":
                m_note = parse_num(props.get("minusNote"))
                p_note = parse_num(props.get("plusNote"))
                if m_note is not None:
                    controls.append({
                        "control": f"{prefix}Pitch Bend -",
                        "module": "PitchBend",
                        "type": "Note",
                        "channel": ch,
                        "number": m_note,
                        "press": "Nudge Backward (NudgeBackward)",
                        "shift_press": "SpeedRange decrease",
                        "long_press": "Continuous nudge backward",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })
                if p_note is not None:
                    controls.append({
                        "control": f"{prefix}Pitch Bend +",
                        "module": "PitchBend",
                        "type": "Note",
                        "channel": ch,
                        "number": p_note,
                        "press": "Nudge Forward (NudgeForward)",
                        "shift_press": "SpeedRange increase",
                        "long_press": "Continuous nudge forward",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "Shift":
                n = parse_num(props.get("note"))
                if n is not None:
                    controls.append({
                        "control": f"{prefix}Shift",
                        "module": "Shift",
                        "type": "Note",
                        "channel": ch,
                        "number": n,
                        "press": "Shift modifier activate (hold)",
                        "shift_press": "None",
                        "long_press": "Shift modifier hold",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "Layer":
                n = parse_num(props.get("note"))
                if n is not None:
                    controls.append({
                        "control": f"{prefix}Layer",
                        "module": "Layer",
                        "type": "Note",
                        "channel": ch,
                        "number": n,
                        "press": "Toggle Layer (Layer A <-> Layer B)",
                        "shift_press": "CycleLayerCount (/GUI/CycleLayerCount)",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "Shortcuts":
                n = parse_num(props.get("note"))
                if n is not None:
                    controls.append({
                        "control": f"{prefix}Shortcuts",
                        "module": "Shortcuts",
                        "type": "Note",
                        "channel": ch,
                        "number": n,
                        "press": "Toggle Shortcuts / Control Center view",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "Menu":
                n = parse_num(props.get("note"))
                if n is not None:
                    controls.append({
                        "control": f"{prefix}Menu",
                        "module": "Menu",
                        "type": "Note",
                        "channel": ch,
                        "number": n,
                        "press": "Toggle Menu / Settings / Control Center",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "Back":
                n = parse_num(props.get("note"))
                if n is not None:
                    controls.append({
                        "control": f"{prefix}Back",
                        "module": "Back",
                        "type": "Note",
                        "channel": ch,
                        "number": n,
                        "press": "Browse Back (Back)",
                        "shift_press": "Browse To Top / Tree root",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "Forward":
                n = parse_num(props.get("note"))
                s_action = resolve_action(props.get("shiftAction", "Action.None"))
                if n is not None:
                    controls.append({
                        "control": f"{prefix}Forward",
                        "module": "Forward",
                        "type": "Note",
                        "channel": ch,
                        "number": n,
                        "press": "Browse Forward / Enter folder (Forward)",
                        # Forward.qml: only shiftAction Quantize changes what Shift does.
                        "shift_press": "Quantize on/off" if s_action == "Quantize" else "Browse Forward (Shift changes nothing)",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": f"shiftAction={s_action}",
                        "source_file": filename
                    })

            elif ntype == "View":
                n = parse_num(props.get("note"))
                h_act = resolve_action(props.get("holdAction", "Action.None"))
                s_act = resolve_action(props.get("shiftAction", "Action.None"))
                sh_act = resolve_action(props.get("shiftHoldAction", "Action.None"))
                d_act = resolve_action(props.get("doubleAction", "Action.None"))
                if n is not None:
                    controls.append({
                        "control": f"{prefix}View",
                        "module": "View",
                        "type": "Note",
                        "channel": ch,
                        "number": n,
                        "press": "View toggle (Performance <-> Library)",
                        "shift_press": f"Shift: {s_act} (SwitchMainViewLayout)",
                        "long_press": f"Hold: {h_act} (ToggleUserProfile / Preferences)",
                        "double_press": f"Double: {d_act} (ShowSoundSwitchView)",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": f"holdAction={h_act}, shiftAction={s_act}, shiftHoldAction={sh_act}, doubleAction={d_act}",
                        "source_file": filename
                    })

            elif ntype in ("BrowseEncoder", "BrowseEncoderJoystick"):
                push_n = parse_num(props.get("pushNote"))
                turn_cc = parse_num(props.get("turnCC"))
                dt_act = resolve_action(props.get("doubleTapAction", "Action.None"))
                s_act = resolve_action(props.get("shiftAction", "Action.None"))
                h_act = resolve_action(props.get("holdAction", "Action.None"))
                if push_n is not None:
                    controls.append({
                        "control": f"{prefix}Browse Encoder Push",
                        "module": ntype,
                        "type": "Note",
                        "channel": ch,
                        "number": push_n,
                        "press": "Load track to deck / Select item (LoadTrack)",
                        "shift_press": "TryUnload (Unload track) / shiftAction",
                        "long_press": "ActionWithGridCueEdit (Grid Edit >= 500 ms)",
                        "double_press": f"DoubleTap: {dt_act} (InstantDouble, window 300 ms)",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": f"doubleTapAction={dt_act}, shiftAction={s_act}, holdAction={h_act}",
                        "source_file": filename
                    })
                if turn_cc is not None:
                    controls.append({
                        "control": f"{prefix}Browse Encoder Turn",
                        "module": ntype,
                        "type": "CC",
                        "channel": ch,
                        "number": turn_cc,
                        "press": "None",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "Scroll tracklist / tree (ScrollIndex)",
                        "shift_turn": "Fast scroll / Letter jump (ShiftIndex; in Grid Edit: Halve/Double tempo)",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "PadModeSelect":
                bm = props.get("buttonsModel")
                notes = []
                if isinstance(bm, dict):
                    for el in extract_list_elements(bm):
                        if "note" in el:
                            notes.append(parse_num(el["note"]))
                labels = ["Hot Cue", "Loop", "Roll", "Slicer"]
                for idx, p_note in enumerate(notes):
                    lbl = labels[idx] if idx < len(labels) else f"Mode {idx}"
                    controls.append({
                        "control": f"{prefix}Pad Mode Button {idx} ({lbl})",
                        "module": "PadModeSelect",
                        "type": "Note",
                        "channel": ch,
                        "number": p_note,
                        "press": f"Select {lbl} mode (repeated press: secondary mode / Stems)",
                        "shift_press": f"PadModeButton{idx}ShiftPressed",
                        "long_press": "None (holdDisabled)",
                        "double_press": "None (two single presses; button 0 enters Stems)",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": f"index={idx}, label={lbl}",
                        "source_file": filename
                    })

            elif ntype in ("ActionPads", "QuadPads"):
                first_n = parse_num(props.get("firstPadNote"))
                count = 4 if ntype == "QuadPads" or "Quad" in str(props) else 8
                if first_n is not None:
                    for i in range(count):
                        pad_num = first_n + i
                        controls.append({
                            "control": f"{prefix}Performance Pad {i+1}",
                            "module": ntype,
                            "type": "Note",
                            "channel": ch,
                            "number": pad_num,
                            "press": f"Pad {i+1} action in active pad mode (Hot Cue, Loop, Roll, Slicer, Stems, Sampler)",
                            "shift_press": f"Pad {i+1} Shift action (Delete Hot Cue, etc.)",
                            "long_press": "Momentary hold in Roll / Slicer / Cue",
                            "double_press": "None",
                            "turn": "None",
                            "shift_turn": "None",
                            "config": f"padIndex={i}",
                            "source_file": filename
                        })

            elif ntype == "JogWheel":
                touch_n = parse_num(props.get("touchNote"))
                cc_up = parse_num(props.get("ccUpper"))
                cc_low = parse_num(props.get("ccLower"))
                has_ts = props.get("hasTrackSearch", "false")
                sens = props.get("jogSensitivity", "")
                if touch_n is not None:
                    controls.append({
                        "control": f"{prefix}Platter Touch",
                        "module": "JogWheel",
                        "type": "Note",
                        "channel": ch,
                        "number": touch_n,
                        "press": "Platter touch on (scratch engage)",
                        "shift_press": "Platter touch (Shift modifier active)",
                        "long_press": "Platter hold (scratch hold)",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": f"hasTrackSearch={has_ts}, jogSensitivity={sens}",
                        "source_file": filename
                    })
                if cc_up is not None and cc_low is not None:
                    controls.append({
                        "control": f"{prefix}Platter Rotation",
                        "module": "JogWheel",
                        "type": "14-bit CC",
                        "channel": ch,
                        "number": f"Upper: {cc_up}, Lower: {cc_low}",
                        "press": "None",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "Pitch bend (Vinyl off) / Scratch (Vinyl on)",
                        "shift_turn": "Track scrub / Search (JogToHoldOutput, if hasTrackSearch)" if has_ts != "false" else "Pitch bend / Scratch",
                        "config": f"hasTrackSearch={has_ts}, jogSensitivity={sens}",
                        "source_file": filename
                    })

            elif ntype == "Motorized":
                controls.append({
                    "control": f"{prefix}Motorized Platter",
                    "module": "Motorized",
                    "type": "Internal / HighSpeedMotor",
                    "channel": ch,
                    "number": "N/A",
                    "press": "Platter touch",
                    "shift_press": "Platter touch shift",
                    "long_press": "Platter hold",
                    "double_press": "None",
                    "turn": "Direct-drive motor rotation & scratch tracking",
                    "shift_turn": "Direct-drive motor rotation",
                    "config": "",
                    "source_file": filename
                })

            elif ntype == "Wheel":
                n = parse_num(props.get("note"))
                s_act = resolve_action(props.get("shiftAction", "Action.None"))
                h_act = resolve_action(props.get("holdAction", "Action.None"))
                if n is not None:
                    controls.append({
                        "control": f"{prefix}Scratch Button",
                        "module": "Wheel",
                        "type": "Note",
                        "channel": ch,
                        "number": n,
                        "press": "Toggle Scratch (Vinyl) Mode",
                        "shift_press": f"Shift: {s_act} (Scratch)",
                        "long_press": f"Hold: {h_act} (GridEdit)",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": f"shiftAction={s_act}, holdAction={h_act}",
                        "source_file": filename
                    })

            elif ntype == "SpeedSlider":
                cc_up = parse_num(props.get("ccUpper"))
                cc_low = parse_num(props.get("ccLower"))
                if cc_up is not None and cc_low is not None:
                    controls.append({
                        "control": f"{prefix}Pitch / Tempo Slider",
                        "module": "SpeedSlider",
                        "type": "14-bit CC",
                        "channel": ch,
                        "number": f"Upper: {cc_up}, Lower: {cc_low}",
                        "press": "None",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "Adjust tempo / playback rate",
                        "shift_turn": "Adjust tempo",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "StopTime":
                cc = parse_num(props.get("cc"))
                if cc is not None:
                    controls.append({
                        "control": f"{prefix}Stop Time Knob",
                        "module": "StopTime",
                        "type": "CC",
                        "channel": ch,
                        "number": cc,
                        "press": "None",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "Adjust vinyl stop brake time",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "Parameter":
                l_note = parse_num(props.get("leftNote"))
                r_note = parse_num(props.get("rightNote"))
                if l_note is not None:
                    controls.append({
                        "control": f"{prefix}Parameter Left",
                        "module": "Parameter",
                        "type": "Note",
                        "channel": ch,
                        "number": l_note,
                        "press": "Decrease loop/beat/FX parameter",
                        "shift_press": "Secondary parameter decrease",
                        "long_press": "Continuous parameter decrease",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })
                if r_note is not None:
                    controls.append({
                        "control": f"{prefix}Parameter Right",
                        "module": "Parameter",
                        "type": "Note",
                        "channel": ch,
                        "number": r_note,
                        "press": "Increase loop/beat/FX parameter",
                        "shift_press": "Secondary parameter increase",
                        "long_press": "Continuous parameter increase",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype in ("Load", "LoadPrepare"):
                n = parse_num(props.get("note"))
                if n is not None:
                    controls.append({
                        "control": f"{prefix}Load Track",
                        "module": ntype,
                        "type": "Note",
                        "channel": ch,
                        "number": n,
                        "press": "Load selected track to deck",
                        "shift_press": "Prepare track / Unload track",
                        "long_press": "None",
                        "double_press": "Instant Double (if hasSideScreens)",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "Media":
                mbm = props.get("mediaButtonsModel")
                if isinstance(mbm, dict):
                    for el in extract_list_elements(mbm):
                        name = el.get("name", "").strip('"')
                        sname = el.get("shiftName", "").strip('"')
                        n = parse_num(el.get("note"))
                        if n is not None:
                            controls.append({
                                "control": f"{prefix}Media Button: {name}",
                                "module": "Media",
                                "type": "Note",
                                "channel": ch,
                                "number": n,
                                "press": f"Media Action: {name}",
                                "shift_press": f"Media Shift Action: {sname}",
                                "long_press": "None",
                                "double_press": "None",
                                "turn": "None",
                                "shift_turn": "None",
                                "config": f"name={name}, shiftName={sname}",
                                "source_file": filename
                            })

            elif ntype == "DeckSelect":
                dsbm = props.get("deckSelectButtonsModel")
                if isinstance(dsbm, dict):
                    for el in extract_list_elements(dsbm):
                        name = el.get("name", "").strip('"')
                        n = parse_num(el.get("note"))
                        if n is not None:
                            controls.append({
                                "control": f"{prefix}Deck Select: {name}",
                                "module": "DeckSelect",
                                "type": "Note",
                                "channel": ch,
                                "number": n,
                                "press": f"Select Deck {name}",
                                "shift_press": "CycleLayerCount / Deck action",
                                "long_press": "None",
                                "double_press": "None",
                                "turn": "None",
                                "shift_turn": "None",
                                "config": f"name={name}",
                                "source_file": filename
                            })

            elif ntype == "MixerChannelCore":
                pfl = parse_num(props.get("pflNote") or props.get("cueNote"))
                fader = parse_num(props.get("faderCC"))
                trim = parse_num(props.get("trimCC") or props.get("gainCC"))
                hi = parse_num(props.get("trebleCC") or props.get("highCC"))
                mid = parse_num(props.get("midCC"))
                low = parse_num(props.get("bassCC") or props.get("lowCC"))
                flt = parse_num(props.get("filterCC"))
                pfl_shift = resolve_action(props.get("pflShiftAction", "Action.None"))

                if pfl is not None:
                    controls.append({
                        "control": f"{prefix}PFL / Cue Button",
                        "module": "MixerChannelCore",
                        "type": "Note",
                        "channel": ch,
                        "number": pfl,
                        "press": "Headphone Cue (PFL) toggle",
                        "shift_press": f"PFL Shift: {pfl_shift} (ToggleChannelStripMode)",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": f"pflShiftAction={pfl_shift}",
                        "source_file": filename
                    })
                if fader is not None:
                    controls.append({
                        "control": f"{prefix}Channel Fader",
                        "module": "MixerChannelCore",
                        "type": "CC",
                        "channel": ch,
                        "number": fader,
                        "press": "None",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "Adjust channel volume fader",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })
                if trim is not None:
                    controls.append({
                        "control": f"{prefix}Trim / Gain Knob",
                        "module": "MixerChannelCore",
                        "type": "CC",
                        "channel": ch,
                        "number": trim,
                        "press": "None",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "Adjust channel input gain / trim",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })
                if hi is not None:
                    controls.append({
                        "control": f"{prefix}EQ High Knob",
                        "module": "MixerChannelCore",
                        "type": "CC",
                        "channel": ch,
                        "number": hi,
                        "press": "None",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "Adjust high frequency EQ / Stems Vocals",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })
                if mid is not None:
                    controls.append({
                        "control": f"{prefix}EQ Mid Knob",
                        "module": "MixerChannelCore",
                        "type": "CC",
                        "channel": ch,
                        "number": mid,
                        "press": "None",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "Adjust mid frequency EQ / Stems Melody",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })
                if low is not None:
                    controls.append({
                        "control": f"{prefix}EQ Low Knob",
                        "module": "MixerChannelCore",
                        "type": "CC",
                        "channel": ch,
                        "number": low,
                        "press": "None",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "Adjust low frequency EQ / Stems Bass",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })
                if flt is not None:
                    controls.append({
                        "control": f"{prefix}Filter / Sweep FX Knob",
                        "module": "MixerChannelCore",
                        "type": "CC",
                        "channel": ch,
                        "number": flt,
                        "press": "None",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "Adjust channel filter / Sweep FX amount",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "Mixer":
                for k, label in [("crossfaderCC", "Crossfader"), ("masterCC", "Master Volume"),
                                 ("boothCC", "Booth Volume"), ("cueMixCC", "Cue Mix"),
                                 ("cueGainCC", "Cue Gain / Headphone Volume"), ("speakerCC", "Speaker Volume")]:
                    if k in props:
                        val = parse_num(props[k])
                        if val is not None:
                            controls.append({
                                "control": f"{prefix}{label}",
                                "module": "Mixer",
                                "type": "CC",
                                "channel": ch,
                                "number": val,
                                "press": "None",
                                "shift_press": "None",
                                "long_press": "None",
                                "double_press": "None",
                                "turn": f"Adjust {label}",
                                "shift_turn": "None",
                                "config": "",
                                "source_file": filename
                            })

            elif ntype == "DJFxSelect":
                bm = props.get("buttonsModel")
                if isinstance(bm, dict):
                    # check onCompleted text
                    c_text = str(bm.get("props", {}))
                    for m in re.finditer(r"note:\s*(\d+),\s*fxName:\s*names\.([a-zA-Z0-9_]+)", c_text):
                        note_num = int(m.group(1))
                        fx_name = m.group(2)
                        controls.append({
                            "control": f"{prefix}FX Select: {fx_name}",
                            "module": "DJFxSelect",
                            "type": "Note",
                            "channel": ch,
                            "number": note_num,
                            "press": f"Select FX {fx_name}",
                            "shift_press": "None",
                            "long_press": "None",
                            "double_press": "None",
                            "turn": "None",
                            "shift_turn": "None",
                            "config": f"fxName={fx_name}",
                            "source_file": filename
                        })

            elif ntype == "DJFxActivate":
                acm = props.get("activateControlsModel")
                act_type = props.get("fxActivateType", "")
                if isinstance(acm, dict):
                    for el in extract_list_elements(acm):
                        m_ch = parse_num(el.get("midiChannel", ch))
                        n = parse_num(el.get("note"))
                        if n is not None:
                            controls.append({
                                "control": f"{prefix}FX Activate ({act_type or 'Button/Paddle'})",
                                "module": "DJFxActivate",
                                "type": "Note",
                                "channel": m_ch,
                                "number": n,
                                "press": "Toggle / Latch FX On/Off",
                                "shift_press": "None",
                                "long_press": "Momentary FX On hold",
                                "double_press": "None",
                                "turn": "None",
                                "shift_turn": "None",
                                "config": f"fxActivateType={act_type}",
                                "source_file": filename
                            })

            elif ntype == "DJFxTime":
                turn_cc = parse_num(props.get("turnCC") or props.get("cc"))
                push_n = parse_num(props.get("pushNote") or props.get("note"))
                if turn_cc is not None:
                    controls.append({
                        "control": f"{prefix}FX Time Knob/Encoder",
                        "module": "DJFxTime",
                        "type": "CC",
                        "channel": ch,
                        "number": turn_cc,
                        "press": "None",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "Adjust FX Beats / Time parameter",
                        "shift_turn": "Fine FX time adjust",
                        "config": "",
                        "source_file": filename
                    })
                if push_n is not None:
                    controls.append({
                        "control": f"{prefix}FX Time Reset / Tap",
                        "module": "DJFxTime",
                        "type": "Note",
                        "channel": ch,
                        "number": push_n,
                        "press": "Tap tempo / reset FX time",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "DJFxWetDry":
                cc = parse_num(props.get("cc"))
                if cc is not None:
                    controls.append({
                        "control": f"{prefix}FX Wet/Dry Knob",
                        "module": "DJFxWetDry",
                        "type": "CC",
                        "channel": ch,
                        "number": cc,
                        "press": "None",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "Adjust FX Wet/Dry balance",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "DJFxFrequency":
                cc = parse_num(props.get("cc"))
                if cc is not None:
                    controls.append({
                        "control": f"{prefix}FX Frequency Knob",
                        "module": "DJFxFrequency",
                        "type": "CC",
                        "channel": ch,
                        "number": cc,
                        "press": "None",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "Adjust FX isolation / frequency filter",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "SweepFxKnob":
                cc = parse_num(props.get("cc"))
                if cc is not None:
                    controls.append({
                        "control": f"{prefix}Sweep FX Knob",
                        "module": "SweepFxKnob",
                        "type": "CC",
                        "channel": ch,
                        "number": cc,
                        "press": "None",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "Adjust Sweep FX amount",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "SweepFxSelect":
                bm = props.get("buttonsModel")
                if isinstance(bm, dict):
                    for el in extract_list_elements(bm):
                        fx_type = el.get("type", "").strip('"')
                        n = parse_num(el.get("note"))
                        if n is not None:
                            controls.append({
                                "control": f"{prefix}Sweep FX Select: {fx_type}",
                                "module": "SweepFxSelect",
                                "type": "Note",
                                "channel": ch,
                                "number": n,
                                "press": f"Select Sweep FX: {fx_type}",
                                "shift_press": "None",
                                "long_press": "None",
                                "double_press": "None",
                                "turn": "None",
                                "shift_turn": "None",
                                "config": f"fxType={fx_type}",
                                "source_file": filename
                            })

            elif ntype == "Quantize":
                n = parse_num(props.get("note"))
                if n is not None:
                    controls.append({
                        "control": f"{prefix}Quantize",
                        "module": "Quantize",
                        "type": "Note",
                        "channel": ch,
                        "number": n,
                        "press": "Quantize On/Off toggle",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            elif ntype == "RemotePlaybackControl":
                sn = parse_num(props.get("startNote"))
                en = parse_num(props.get("stopNote"))
                if sn is not None:
                    controls.append({
                        "control": f"{prefix}Remote Play",
                        "module": "RemotePlaybackControl",
                        "type": "Note",
                        "channel": ch,
                        "number": sn,
                        "press": "Remote Playback Start",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })
                if en is not None:
                    controls.append({
                        "control": f"{prefix}Remote Stop",
                        "module": "RemotePlaybackControl",
                        "type": "Note",
                        "channel": ch,
                        "number": en,
                        "press": "Remote Playback Stop",
                        "shift_press": "None",
                        "long_press": "None",
                        "double_press": "None",
                        "turn": "None",
                        "shift_turn": "None",
                        "config": "",
                        "source_file": filename
                    })

            # Recurse children
            for child in children:
                process_node(child, current_scope)

        for top_child in root.get("children", []):
            process_node(top_child, {"channel": global_midi_channel})

    return controls

def format_markdown_table(board_name, controls):
    lines = []
    lines.append(f"# Control Surface Mapping: {board_name}\n")
    lines.append(f"Total mapped controls: {len(controls)}\n")
    lines.append("| Control / Role | Module | MIDI Type | Ch | Number | Press | Shift + Press | Long Press | Double Press | Turn | Shift + Turn | Config / Notes |")
    lines.append("|---|---|---|---|---|---|---|---|---|---|---|---|")
    for c in controls:
        num_str = str(c["number"])
        if isinstance(c["number"], int):
            num_str = f"{c['number']} (0x{c['number']:02x})"
        lines.append(
            f"| {c['control']} | {c['module']} | {c['type']} | {c['channel']} | {num_str} | "
            f"{c['press']} | {c['shift_press']} | {c['long_press']} | {c['double_press']} | "
            f"{c['turn']} | {c['shift_turn']} | {c['config']} |"
        )
    lines.append("")
    return "\n".join(lines)

def add_ids(controls):
    """Give every control a stable id: <module>:<note> or <module>:cc<cc>. A repeat
    (the same module and number twice) gets a #2 so ids stay unique per board."""
    seen = {}
    for c in controls:
        n = c["number"]
        if not isinstance(n, int):  # a 14-bit pair "Upper: 17, Lower: 49" becomes 17+49
            n = "+".join(re.findall(r"\d+", str(n)))
        base = "%s:%s%s" % (c["module"], "cc" if "CC" in c["type"] else "", n)
        seen[base] = seen.get(base, 0) + 1
        c["id"] = base if seen[base] == 1 else "%s#%d" % (base, seen[base])
    return controls


def main():
    here = os.path.dirname(os.path.abspath(__file__))
    parser = argparse.ArgumentParser(description="Inventory Engine OS controller assignments")
    parser.add_argument("--assignments", required=True,
                        help="PresetAssignmentFiles directory of the unpacked 5.3.0 firmware")
    parser.add_argument("--qrc", required=True, help="Extracted qrc directory of the same Engine")
    parser.add_argument("--out", default=os.path.join(here, "..", "..", "pkg", "mapping", "data"),
                        help="Output directory (default: pkg/mapping/data, which the console embeds)")
    parser.add_argument("--boards", nargs="*", default=None,
                        help="Specific boards to inventory (default: all)")
    parser.add_argument("--markdown", action="store_true", help="also write a Markdown table per board")
    args = parser.parse_args()

    os.makedirs(args.out, exist_ok=True)
    all_board_dirs = sorted(glob.glob(os.path.join(args.assignments, "*")))

    boards_to_run = []
    for b_dir in all_board_dirs:
        b_name = os.path.basename(b_dir)
        if args.boards and b_name not in args.boards:
            continue
        boards_to_run.append((b_name, b_dir))

    summary = []
    for b_name, b_dir in boards_to_run:
        ctrls = add_ids(build_board_inventory(b_name, b_dir, args.qrc))
        json_path = os.path.join(args.out, f"{b_name}.json")
        with open(json_path, "w") as f:
            json.dump({"board": b_name, "controls_count": len(ctrls), "controls": ctrls}, f, indent=1)
            f.write("\n")
        if args.markdown:
            with open(os.path.join(args.out, f"{b_name}.md"), "w") as f:
                f.write(format_markdown_table(b_name, ctrls))
        summary.append(f"{b_name:8s}: {len(ctrls):3d} controls -> {os.path.normpath(json_path)}")

    print("=== Inventory Execution Summary ===")
    for s in summary:
        print(s)

if __name__ == "__main__":
    main()
