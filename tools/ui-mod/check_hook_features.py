#!/usr/bin/env python3
"""Fail the build when a native edit, instruction, patch call or wrapper lacks a feature."""
import re
import sys
from pathlib import Path


def uncomment(text):
    return re.sub(r'"(?:\\.|[^"\\])*"|/\*.*?\*/|//[^\n]*', lambda m: m[0] if m[0].startswith(chr(34)) else '', text, flags=re.S)


def elements(text):
    depth = 0
    start = 0
    for i, ch in enumerate(text):
        if ch in '([{':
            depth += 1
        elif ch in ')]}':
            depth -= 1
        elif ch == ',' and depth == 0:
            yield text[start:i].strip()
            start = i + 1
    if text[start:].strip():
        yield text[start:].strip()


def check(source):
    errors = []
    def fail(pos, message):
        errors.append(f'{source.count(chr(10), 0, pos) + 1}: {message}')
    # Positional records must spell the last field, including intentional ALWAYS.
    for table in re.finditer(r'static const struct engine_edit (\w+)\[\] = \{(.*?)^\};', source, re.S | re.M):
        for row in re.finditer(r'\{([^{}]*)\}', uncomment(table[2])):
            fields = list(elements(row[1]))
            if len(fields) != 6 or not re.fullmatch(r'HF_[A-Z_0-9]+', fields[-1]):
                fail(table.start(), f'{table[1]} has an untagged edit')
    # Patch primitives have a mandatory tag in their C signature as well.
    for call in re.finditer(r'\b(?:try_patch_bytes|patch_bytes|patch_pointer|redirect_function)\(\s*([^,\n]+)', source):
        arg = call[1].strip()
        if arg == 'feature':
            enclosing = re.findall(r'^static [^;{\n]*?\b(\w+)\([^;{]*?\)\s*\{', source[:call.start()], re.M)
            if not enclosing or enclosing[-1] not in ('patch_bytes', 'redirect_function'):
                fail(call.start(), 'only the central dispatchers may forward a feature variable')
        if arg not in ('int feature', 'feature') and not re.fullmatch(r'HF_[A-Z_0-9]+', arg):
            fail(call.start(), 'native patch call needs an explicit HF_* argument')
    start = source.index('A32_ONLY static void prepare_config(')
    end = source.index('\nA32_ONLY static void prepare_stems_feature', start)
    config = uncomment(source[start:end])
    for array in re.finditer(r'(?:const )?uint32_t (\w+)\[\] = \{(.*?)\};', config, re.S):
        for word in elements(array[2]):
            if not re.match(r'^HF_CONFIG\(HF_[A-Z_0-9]+,', word):
                fail(start, f'getConfig array {array[1]} has an untagged instruction')
    for write in re.finditer(r'\bcode\[[^;\n]*?\]\s*=\s*([^;\n]+)', config):
        if write[1].lstrip().startswith('{'):
            continue  # the array above
        if not re.match(r'HF_CONFIG\(HF_[A-Z_0-9]+,', write[1]):
            fail(start, 'getConfig slot write needs HF_CONFIG')
    if re.search(r'\bcode\[[^;\n]*?\]\s*(?:[|&+^\-]=|\+\+|--)', config):
        fail(start, 'getConfig compound writes must use a tagged instruction assignment')
    if re.search(r'\bmem(?:cpy|move)\(\s*code\b', config):
        fail(start, 'getConfig copy needs HF_CONFIG_COPY')
    # Directly exported interposers (including macro-generated open/stat aliases).
    signature = re.compile(r'^([A-Za-z_][\w *]*?\b(\w+)\([^;{}]*?\)\s*\{)', re.M)
    clean = re.sub(r'"(?:\\.|[^"\\])*"|\x27(?:\\.|[^\x27\\])*\x27|/\*.*?\*/|//[^\n]*', lambda m: re.sub(r'[^\n]', ' ', m[0]), source, flags=re.S)
    for fn in signature.finditer(source):
        if re.search(r'\b(?:static|typedef)\b', fn[1]):
            continue
        lines = source[:fn.start()].rstrip().splitlines()
        while lines and lines[-1].startswith('__attribute__'):
            lines.pop()
        previous = lines[-1].rstrip('\\').rstrip() if lines else ''
        if not re.fullmatch(r'HF_INTERPOSE\(HF_[A-Z_0-9]+\)', previous):
            fail(fn.start(), f'exported hook {fn[2]} needs HF_INTERPOSE')
    for ctor in re.finditer(r'__attribute__\(\(constructor\)\) static void (\w+)\(', source):
        previous = source[:ctor.start()].rstrip().splitlines()[-1]
        if not re.fullmatch(r'HF_INTERPOSE\(HF_[A-Z_0-9]+\)', previous):
            fail(ctor.start(), f'constructor {ctor[1]} needs HF_INTERPOSE')
    # An added writable-text path must use the feature-tagged primitive, not a new installer.
    fn_header = re.compile(r'^[^\n;{}]*\b(\w+)\([^;{}]*?\)\s*\{', re.M)
    functions = []
    for fn in fn_header.finditer(clean):
        if fn[1] in ('if', 'for', 'while', 'switch'):
            continue
        opening = clean.index('{', fn.start())
        depth = 1
        end = opening + 1
        while depth and end < len(clean):
            depth += (clean[end] == '{') - (clean[end] == '}')
            end += 1
        body = clean[opening:end]
        functions.append((fn[1], fn.start(), opening, end, body))
        if 'mprotect(' in body and fn[1] not in ('rx_code', 'try_patch_bytes', 'patch_pointer', 'apply_engine_edits', 'oa_install'):
            fail(fn.start(), f'{fn[1]} must use a tagged patch primitive')
        if fn[1] == 'oa_install' and re.search(r'mprotect\((?!stub,)', body):
            fail(fn.start(), 'OfflineAnalyzer text writes must use a tagged patch primitive')
    errors.extend(check_az04_parity(source, clean, functions))
    return errors


def _nocomment_preserve_strings(source):
    return re.sub(
        r'"(?:\\.|[^"\\])*"|\x27(?:\\.|[^\x27\\])*\x27|/\*.*?\*/|//[^\n]*',
        lambda m: m[0] if m[0][0] in ('"', "'") else re.sub(r'[^\n]', ' ', m[0]),
        source,
        flags=re.S,
    )


def _line_offsets(source):
    offsets = [0]
    for line in source.splitlines(True):
        offsets.append(offsets[-1] + len(line))
    return offsets


def _line_of_pos(offsets, pos):
    lo, hi = 0, len(offsets) - 2
    while lo < hi:
        mid = (lo + hi + 1) // 2
        if offsets[mid] <= pos:
            lo = mid
        else:
            hi = mid - 1
    return lo


def _classify_pp_expr(expr):
    if re.search(r'!\s*defined\s*\(\s*__aarch64__\s*\)', expr):
        return 'a32'
    if re.search(r'\bdefined\s*\(\s*__aarch64__\s*\)', expr):
        return 'a64'
    return 'other'


def _line_archs(nocomment_lines):
    stack = []
    archs = []
    in_pp_cont = False
    for line in nocomment_lines:
        if in_pp_cont:
            archs.append('pp')
            in_pp_cont = line.rstrip().endswith('\\')
            continue
        s = line.strip()
        if s.startswith('#'):
            m = re.match(r'^#\s*(if|ifdef|ifndef|elif|else|endif)\b(.*)$', s)
            if m:
                kw, rest = m[1], m[2].strip()
                if kw == 'if':
                    stack.append(_classify_pp_expr(rest))
                elif kw == 'ifdef':
                    stack.append('a64' if rest == '__aarch64__' else 'other')
                elif kw == 'ifndef':
                    stack.append('a32' if rest == '__aarch64__' else 'other')
                elif kw == 'elif' and stack:
                    stack[-1] = _classify_pp_expr(rest)
                elif kw == 'else' and stack:
                    if stack[-1] == 'a32':
                        stack[-1] = 'a64'
                    elif stack[-1] == 'a64':
                        stack[-1] = 'a32'
                elif kw == 'endif' and stack:
                    stack.pop()
            archs.append('pp')
            in_pp_cont = line.rstrip().endswith('\\')
            continue
        if 'a32' in stack:
            archs.append('a32')
        elif 'a64' in stack:
            archs.append('a64')
        else:
            archs.append('common')
    return archs


def _line_pp_branches(nocomment_lines):
    stack = []
    branches = []
    next_id = 0
    in_pp_cont = False
    for line in nocomment_lines:
        if in_pp_cont:
            branches.append(None)
            in_pp_cont = line.rstrip().endswith('\\')
            continue
        s = line.strip()
        if s.startswith('#'):
            m = re.match(r'^#\s*(if|ifdef|ifndef|elif|else|endif)\b', s)
            if m:
                kw = m[1]
                if kw in ('if', 'ifdef', 'ifndef'):
                    next_id += 1
                    stack.append([next_id, 0])
                elif kw in ('elif', 'else') and stack:
                    stack[-1][1] += 1
                elif kw == 'endif' and stack:
                    stack.pop()
            branches.append(None)
            in_pp_cont = line.rstrip().endswith('\\')
            continue
        branches.append(tuple(tuple(frame) for frame in stack))
    return branches


_EXEMPT_STR_RE = re.compile(r'\bAZ04_EXEMPT\s*\(\s*"((?:\\.|[^"\\])*)"\s*\)')


def _valid_exempt_matches(line):
    return [m for m in _EXEMPT_STR_RE.finditer(line) if m[1].strip()]


def _valid_az04_exempt_on_line(line):
    matches = _valid_exempt_matches(line)
    # Remove valid AZ04_EXEMPT("non-empty") annotations; any remaining AZ04_EXEMPT token is malformed.
    stripped = _EXEMPT_STR_RE.sub(lambda m: '' if m[1].strip() else m[0], line)
    if re.search(r'\bAZ04_EXEMPT\b', stripped):
        return False, True
    return bool(matches), False


def _is_standalone_exempt_line(line):
    matches = _valid_exempt_matches(line)
    if len(matches) != 1:
        return None
    stripped = _EXEMPT_STR_RE.sub('', line)
    if stripped.strip():
        return None
    return matches[0]


def _consume_exempt_for_def(nocomment_lines, clean_lines, archs, branches, hdr_idx, open_idx, fn_name, consumed):
    fn_re = re.compile(rf'\b{re.escape(fn_name)}\s*\(')
    target_branch = branches[open_idx]
    for li in range(hdr_idx, open_idx + 1):
        if branches[li] != target_branch or any(archs[k] == 'pp' for k in range(li + 1, open_idx + 1)):
            continue
        standalone = _is_standalone_exempt_line(nocomment_lines[li])
        if standalone is not None:
            if (li, standalone.start()) not in consumed:
                consumed.add((li, standalone.start()))
                return True
            continue
        fn_m = fn_re.search(clean_lines[li])
        if not fn_m:
            continue
        seg_start = max(
            clean_lines[li].rfind(';', 0, fn_m.start()),
            clean_lines[li].rfind('{', 0, fn_m.start()),
            clean_lines[li].rfind('}', 0, fn_m.start()),
        ) + 1
        candidates = [
            m for m in _valid_exempt_matches(nocomment_lines[li])
            if seg_start <= m.start() and m.end() <= fn_m.start() and (li, m.start()) not in consumed
        ]
        if candidates:
            consumed.add((li, candidates[-1].start()))
            return True
    p = hdr_idx - 1
    while p >= 0 and not nocomment_lines[p].strip():
        if archs[p] == 'pp':
            p = -1
            break
        p -= 1
    if p >= 0 and archs[p] != 'pp' and branches[p] == target_branch:
        standalone = _is_standalone_exempt_line(nocomment_lines[p])
        if standalone is not None and (p, standalone.start()) not in consumed:
            consumed.add((p, standalone.start()))
            return True
    return False


def _consume_exempt_for_call(nocomment_lines, clean_lines, archs, branches, li, call_col, consumed):
    seg_start = max(
        clean_lines[li].rfind(';', 0, call_col),
        clean_lines[li].rfind('{', 0, call_col),
        clean_lines[li].rfind('}', 0, call_col),
    ) + 1
    inline = [
        m for m in _valid_exempt_matches(nocomment_lines[li])
        if seg_start <= m.start() and m.end() <= call_col and (li, m.start()) not in consumed
    ]
    if inline:
        consumed.add((li, inline[-1].start()))
        return True
    if seg_start == 0:
        p = li - 1
        while p >= 0 and not nocomment_lines[p].strip():
            if archs[p] == 'pp':
                p = -1
                break
            p -= 1
        if p >= 0 and archs[p] != 'pp' and branches[p] == branches[li]:
            standalone = _is_standalone_exempt_line(nocomment_lines[p])
            if standalone is not None and (p, standalone.start()) not in consumed:
                consumed.add((p, standalone.start()))
                return True
    return False


def _fn_name_before_brace(text):
    s = text.rstrip()
    # Skip trailing GCC asm/attribute clauses before '{'.
    while s.endswith(')'):
        depth = 0
        pos = len(s) - 1
        while pos >= 0:
            depth += (s[pos] == ')') - (s[pos] == '(')
            if depth == 0:
                break
            pos -= 1
        if pos < 0:
            return None
        prefix = s[:pos].rstrip()
        m_kw = re.search(r'\b([A-Za-z_]\w*)$', prefix)
        if not m_kw:
            return None
        kw = m_kw[1]
        if kw in ('__asm__', 'asm', '__attribute__', 'sizeof'):
            s = prefix[:m_kw.start()].rstrip()
            continue
        return kw
    return None


def _top_level_functions(nocomment_lines, clean_lines, archs, branches):
    functions = []
    depth = 0
    in_fn = None
    for idx, cline in enumerate(clean_lines):
        if archs[idx] == 'pp':
            continue
        opens = cline.count('{')
        closes = cline.count('}')
        if depth == 0 and opens > 0 and in_fn is None:
            hdr_lines = []
            hdr_start = idx
            for back in range(idx, max(-1, idx - 6), -1):
                if archs[back] == 'pp':
                    break
                t = clean_lines[back]
                if back < idx and (';' in t or '}' in t or '{' in t):
                    break
                hdr_lines.append(t)
                hdr_start = back
            hdr = ' '.join(reversed(hdr_lines))
            before_brace = hdr.split('{', 1)[0]
            name = _fn_name_before_brace(before_brace)
            if name and name not in ('if', 'for', 'while', 'switch', 'sizeof', '__attribute__', '_Static_assert'):
                hdr_nocom = '\n'.join(
                    nocomment_lines[li]
                    for li in range(max(0, hdr_start - 1), idx + 1)
                    if archs[li] != 'pp' and branches[li] == branches[idx]
                )
                is_a32_only = bool(re.search(r'\bA32_ONLY\b', hdr_nocom)) or archs[idx] == 'a32'
                f_arch = 'a32' if is_a32_only else archs[idx]
                in_fn = [name, hdr_start, idx, f_arch]
        depth += opens - closes
        if depth <= 0:
            depth = 0
            if in_fn is not None:
                functions.append((in_fn[0], in_fn[1], in_fn[2], idx, in_fn[3]))
                in_fn = None
    return functions


def check_az04_parity(source, clean, _functions):
    errors = []
    offsets = _line_offsets(source)
    nocomment = _nocomment_preserve_strings(source)
    nocomment_lines = nocomment.splitlines()
    clean_lines = clean.splitlines()
    archs = _line_archs(nocomment_lines)
    branches = _line_pp_branches(nocomment_lines)

    def fail_line(idx, message):
        errors.append(f'{idx + 1}: {message}')

    # Validate every AZ04_EXEMPT("reason") has a non-empty string reason.
    for idx, line in enumerate(nocomment_lines):
        if re.search(r'^#\s*define\s+AZ04_EXEMPT\b', line):
            continue
        if re.search(r'\bAZ04_EXEMPT\b', clean_lines[idx]):
            ok, bad = _valid_az04_exempt_on_line(line)
            if bad or not ok:
                fail_line(idx, 'AZ04_EXEMPT requires a non-empty string literal reason')

    # Classify top-level function spans and check A32-only installer definitions + calls.
    fn_info = _top_level_functions(nocomment_lines, clean_lines, archs, branches)
    fn_arch_by_line = {}
    installer_archs = {}
    for name, hdr_idx, open_idx, end_idx, f_arch in fn_info:
        for li in range(hdr_idx, end_idx + 1):
            fn_arch_by_line[li] = f_arch
        if re.fullmatch(r'(?:install|prepare)_[a-z0-9_]+', name):
            installer_archs.setdefault(name, set()).add(f_arch)

    consumed_exempts = set()
    for name, hdr_idx, open_idx, _end_idx, f_arch in fn_info:
        if f_arch != 'a32' or not re.fullmatch(r'(?:install|prepare)_[a-z0-9_]+', name):
            continue
        if 'a64' in installer_archs[name] or 'common' in installer_archs[name]:
            continue
        if not _consume_exempt_for_def(nocomment_lines, clean_lines, archs, branches, hdr_idx, open_idx, name, consumed_exempts):
            fail_line(open_idx, f'A32-only installer {name} needs AZ04_EXEMPT("reason") or an AZ04 implementation')

    call_re = re.compile(r'\b((?:install|prepare)_[a-z0-9_]+)\s*\(')
    for fn_name, _hdr_idx, open_idx, end_idx, f_arch in fn_info:
        if f_arch != 'common':
            continue
        calls = []
        for li in range(open_idx + 1, end_idx + 1):
            for m in call_re.finditer(clean_lines[li]):
                calls.append((m[1], li, m.start()))
        non_a32 = {c for c, li, _col in calls if archs[li] in ('a64', 'common')}
        for callee, li, call_col in calls:
            if archs[li] == 'a32' and callee not in non_a32 and not _consume_exempt_for_call(
                nocomment_lines, clean_lines, archs, branches, li, call_col, consumed_exempts
            ):
                fail_line(li, f'A32-only call to {callee} in {fn_name} needs AZ04_EXEMPT("reason") or an AZ04 call')

    # Check A32-only HF_* features against AZ04/common sites or az04_exempt_features[].
    exempt_table_lines = set()
    exempt_features = set()
    ex_m = re.search(
        r'static\s+const\s+int\s+az04_exempt_features\[\][^{=]*=\s*\{(.*?)^\s*\};',
        nocomment,
        re.S | re.M,
    )
    if ex_m:
        ex_s = _line_of_pos(offsets, ex_m.start())
        ex_e = _line_of_pos(offsets, ex_m.end() - 1)
        exempt_table_lines.update(range(ex_s, ex_e + 1))
        for li in range(ex_s + 1, ex_e):
            line = nocomment_lines[li]
            fms = list(re.finditer(r'\bHF_([A-Z_0-9]+)\b(?!\s*\()', line))
            if not fms:
                continue
            avail = [m for m in _valid_exempt_matches(line) if (li, m.start()) not in consumed_exempts]
            for fm in fms:
                fid = fm[1]
                if fid in ('ALWAYS', 'COUNT'):
                    fail_line(li, f'HF_{fid} does not belong in az04_exempt_features')
                    continue
                if avail:
                    m = avail.pop(0)
                    consumed_exempts.add((li, m.start()))
                    exempt_features.add(fid)
                else:
                    fail_line(li, f'az04_exempt_features entry HF_{fid} needs AZ04_EXEMPT("reason")')

    a32_features = {}
    az04_features = set()
    for li, line in enumerate(clean_lines):
        if li in exempt_table_lines:
            continue
        eff = 'a32' if fn_arch_by_line.get(li) == 'a32' else archs[li]
        if eff == 'pp':
            continue
        for fm in re.finditer(r'\bHF_([A-Z_0-9]+)\b(?!\s*\()', line):
            fid = fm[1]
            if fid in ('ALWAYS', 'COUNT'):
                continue
            if eff == 'a32':
                a32_features.setdefault(fid, li)
            else:
                az04_features.add(fid)

    for fid, first_li in sorted(a32_features.items()):
        if fid not in az04_features and fid not in exempt_features:
            fail_line(
                first_li,
                f'HF_{fid} has no AZ04 site and is not documented in az04_exempt_features[]',
            )
    errors.extend(_check_az04_build_descriptors(nocomment, nocomment_lines, clean_lines, offsets, archs, fn_info))
    return errors


def _split_top_level_commas(text):
    items = []
    depth = 0
    in_quote = None
    escaped = False
    start = 0
    for i, ch in enumerate(text):
        if in_quote:
            if escaped:
                escaped = False
            elif ch == '\\':
                escaped = True
            elif ch == in_quote:
                in_quote = None
            continue
        if ch in ('"', "'"):
            in_quote = ch
        elif ch in '([{':
            depth += 1
        elif ch in ')]}':
            depth -= 1
        elif ch == ',' and depth == 0:
            part = text[start:i].strip()
            if part:
                items.append(part)
            start = i + 1
    tail = text[start:].strip()
    if tail:
        items.append(tail)
    return items


def _extract_brace_block(text, open_pos):
    depth = 0
    in_quote = None
    escaped = False
    for i in range(open_pos, len(text)):
        ch = text[i]
        if in_quote:
            if escaped:
                escaped = False
            elif ch == '\\':
                escaped = True
            elif ch == in_quote:
                in_quote = None
            continue
        if ch in ('"', "'"):
            in_quote = ch
        elif ch == '{':
            depth += 1
        elif ch == '}':
            depth -= 1
            if depth == 0:
                return text[open_pos + 1:i], i
    return None, -1


def _parse_engine_build_struct(nocomment):
    m = re.search(r'\bstruct\s+engine_build\s*\{', nocomment)
    if not m:
        return set(), 21
    body, _ = _extract_brace_block(nocomment, m.end() - 1)
    if body is None:
        return set(), 21
    dsp_m = re.search(r'\buint32_t\s+dsp\s*\[\s*(\d+)\s*\]', body)
    dsp_count = int(dsp_m[1]) if dsp_m else 21
    fields = set()
    for stmt in body.split(';'):
        stmt = stmt.strip()
        if not stmt:
            continue
        for decl in stmt.split(','):
            decl = re.sub(r'\[[^\]]*\]', '', decl).strip()
            fm = re.search(r'\b([A-Za-z_]\w*)$', decl)
            if fm:
                fields.add(fm[1])
    return fields, dsp_count


def _required_az04_build_fields(nocomment_lines, clean_lines, archs, fn_info, struct_fields):
    required = {
        'name', 'id', 'dsp', 'edits', 'edit_count',
        'sqlite_api', 'sqlite_exec', 'sqlite_open', 'sqlite_open_v2', 'sqlite_opendb', 'sqlite_prefix',
        'bt_send', 'tidal_sso_slot', 'tidal_sso_getter',
        'shadow_vtable_a', 'shadow_vtable_b', 'shadow_vtable_slot', 'shadow_position_fn',
        'shadow_array_offset', 'shadow_position_prefix_size', 'shadow_position_prefix',
        'shadow_owner_slot', 'shadow_owner_fn',
    }
    a32_only_fields = {'shadow_capture_site', 'shadow_capture_prefix'}
    for fn_name, hdr_idx, open_idx, end_idx, f_arch in fn_info:
        if f_arch not in ('common', 'a64'):
            continue
        fn_span = '\n'.join(clean_lines[li] for li in range(hdr_idx, end_idx + 1) if archs[li] != 'a32')
        has_b_ptr = bool(re.search(r'\bstruct\s+engine_build\s*\*\s*b\b', fn_span))
        for fm in re.finditer(r'\bbuild->([A-Za-z_]\w*)', fn_span):
            if fm[1] in struct_fields:
                required.add(fm[1])
        if has_b_ptr:
            for fm in re.finditer(r'\bb->([A-Za-z_]\w*)', fn_span):
                if fm[1] in struct_fields:
                    required.add(fm[1])
    return required - a32_only_fields


def _is_zero_scalar(expr):
    s = expr.strip()
    while s.startswith('(') and s.endswith(')'):
        inner = s[1:-1].strip()
        if not inner:
            break
        s = inner
    if re.fullmatch(r'(?:NULL|\(\s*void\s*\*\s*\)\s*0+)', s):
        return True
    m = re.fullmatch(r'([+-]?(?:0[xX][0-9A-Fa-f]+|0[bB][01]+|\d+))[uUlL]*', s)
    if m:
        return int(m[1], 0) == 0
    return False


def _is_c_string_expr(expr):
    return bool(re.fullmatch(r'(?:"(?:\\.|[^"\\])*"\s*)+', expr.strip()))


def _c_string_nonempty(expr):
    parts = re.findall(r'"((?:\\.|[^"\\])*)"', expr.strip())
    return bool(''.join(parts))


def _check_az04_build_descriptors(nocomment, nocomment_lines, clean_lines, offsets, archs, fn_info):
    errors = []
    struct_fields, dsp_count = _parse_engine_build_struct(nocomment)
    required = _required_az04_build_fields(nocomment_lines, clean_lines, archs, fn_info, struct_fields)

    a64_tables = []
    for tm in re.finditer(r'\bstatic\s+const\s+struct\s+engine_build\s+engine_builds\s*\[\s*\]\s*=\s*\{', nocomment):
        decl_li = _line_of_pos(offsets, tm.start())
        if archs[decl_li] == 'a64':
            a64_tables.append((tm, decl_li))

    if not a64_tables:
        errors.append('1: missing AArch64 engine_builds[] table for AZ04')
        return errors

    row_count = 0
    for tm, decl_li in a64_tables:
        table_body, _ = _extract_brace_block(nocomment, tm.end() - 1)
        if table_body is None:
            errors.append(f'{decl_li + 1}: unterminated AArch64 engine_builds[] initializer')
            continue
        for entry in _split_top_level_commas(table_body):
            if not entry.startswith('{') or not entry.endswith('}'):
                errors.append(f'{decl_li + 1}: AArch64 engine_builds[] entry must be a brace initializer')
                continue
            row_count += 1
            entry_body = entry[1:-1]
            fields = {}
            bad_item = False
            for item in _split_top_level_commas(entry_body):
                im = re.fullmatch(r'\s*\.([A-Za-z_]\w*)\s*=\s*(.+)', item, re.S)
                if not im:
                    errors.append(f'{decl_li + 1}: AArch64 engine_builds[] entry must use designated .field = value initializers')
                    bad_item = True
                    continue
                fname, fval = im[1], im[2].strip()
                if fname in fields:
                    errors.append(f'{decl_li + 1}: AArch64 engine_builds[] has duplicate .{fname}')
                fields[fname] = fval
            if bad_item:
                continue

            name_val = fields.get('name', '')
            if _is_c_string_expr(name_val) and _c_string_nonempty(name_val):
                build_name = ''.join(re.findall(r'"((?:\\.|[^"\\])*)"', name_val))
            else:
                build_name = '<unnamed>'

            for field in sorted(required):
                if field not in fields:
                    errors.append(f'{decl_li + 1}: AZ04 build "{build_name}" missing required descriptor field .{field}')
                    continue
                val = fields[field]
                if field == 'dsp':
                    if not (val.startswith('{') and val.endswith('}')):
                        errors.append(f'{decl_li + 1}: AZ04 build "{build_name}" .dsp must be a brace initializer')
                    else:
                        slots = _split_top_level_commas(val[1:-1])
                        if len(slots) != dsp_count or any(_is_zero_scalar(s) for s in slots):
                            errors.append(f'{decl_li + 1}: AZ04 build "{build_name}" .dsp must provide {dsp_count} non-zero offsets')
                elif field == 'sqlite_prefix':
                    if not (val.startswith('{') and val.endswith('}')):
                        errors.append(f'{decl_li + 1}: AZ04 build "{build_name}" .sqlite_prefix must be a brace initializer')
                    else:
                        slots = _split_top_level_commas(val[1:-1])
                        if len(slots) < 2 or not _is_c_string_expr(slots[1]) or not _c_string_nonempty(slots[1]):
                            errors.append(f'{decl_li + 1}: AZ04 build "{build_name}" .sqlite_prefix[1] (sqlite_opendb prologue) must be non-empty')
                elif field == 'shadow_position_prefix':
                    if not (val.startswith('{') and val.endswith('}')):
                        errors.append(f'{decl_li + 1}: AZ04 build "{build_name}" .shadow_position_prefix must be a brace initializer')
                    else:
                        bytes_items = _split_top_level_commas(val[1:-1])
                        size_expr = fields.get('shadow_position_prefix_size', '0')
                        size_m = re.fullmatch(r'(0[xX][0-9A-Fa-f]+|\d+)[uUlL]*', size_expr.strip())
                        min_len = int(size_m[1], 0) if size_m else 1
                        if len(bytes_items) < max(1, min_len) or all(_is_zero_scalar(b) for b in bytes_items):
                            errors.append(f'{decl_li + 1}: AZ04 build "{build_name}" .shadow_position_prefix must be non-zero')
                elif val.startswith('{') and val.endswith('}'):
                    elems = _split_top_level_commas(val[1:-1])
                    if not elems or all(_is_zero_scalar(e) for e in elems):
                        errors.append(f'{decl_li + 1}: AZ04 build "{build_name}" has zero/empty required descriptor field .{field}')
                elif _is_c_string_expr(val):
                    if not _c_string_nonempty(val):
                        errors.append(f'{decl_li + 1}: AZ04 build "{build_name}" has empty string in required descriptor field .{field}')
                elif _is_zero_scalar(val):
                    errors.append(f'{decl_li + 1}: AZ04 build "{build_name}" has zero/empty required descriptor field .{field}')

    if row_count == 0:
        errors.append(f'{a64_tables[0][1] + 1}: AArch64 engine_builds[] must list at least one AZ04 build')
    return errors


if __name__ == '__main__':
    path = Path(sys.argv[1]) if len(sys.argv) > 1 else Path(__file__).with_name('engine_ui_hook.c')
    errors = check(path.read_text())
    if errors:
        print('\n'.join(f'{path}:{e}' for e in errors), file=sys.stderr)
        sys.exit(1)
    print('hook feature coverage: PASS')
