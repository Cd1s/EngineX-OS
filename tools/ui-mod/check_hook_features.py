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
    for line in nocomment_lines:
        s = line.strip()
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
            continue
        if 'a32' in stack:
            archs.append('a32')
        elif 'a64' in stack:
            archs.append('a64')
        else:
            archs.append('common')
    return archs


def _valid_az04_exempt_on_line(line):
    matches = list(re.finditer(r'\bAZ04_EXEMPT\s*\(([^)]*)\)', line))
    if not matches:
        return False, False
    for m in matches:
        arg = m[1].strip()
        s = re.fullmatch(r'"((?:\\.|[^"\\])*)"', arg)
        if not s or not s[1].strip():
            return False, True
    return True, False


def _has_az04_exempt(nocomment_lines, start_idx, end_idx=None):
    if end_idx is None:
        end_idx = start_idx
    for idx in range(start_idx, end_idx + 1):
        ok, _ = _valid_az04_exempt_on_line(nocomment_lines[idx])
        if ok:
            return True
    p = start_idx - 1
    while p >= 0 and not nocomment_lines[p].strip():
        p -= 1
    if p >= 0:
        ok, _ = _valid_az04_exempt_on_line(nocomment_lines[p])
        if ok:
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


def _top_level_functions(nocomment_lines, clean_lines, archs):
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
                    continue
                t = clean_lines[back]
                if back < idx and (';' in t or '}' in t or '{' in t):
                    break
                hdr_lines.append(t)
                hdr_start = back
            hdr = ' '.join(reversed(hdr_lines))
            before_brace = hdr.split('{', 1)[0]
            name = _fn_name_before_brace(before_brace)
            if name and name not in ('if', 'for', 'while', 'switch', 'sizeof', '__attribute__', '_Static_assert'):
                hdr_nocom = '\n'.join(nocomment_lines[max(0, hdr_start - 1):idx + 1])
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

    def fail_line(idx, message):
        errors.append(f'{idx + 1}: {message}')

    # Validate every AZ04_EXEMPT("reason") has a non-empty string reason.
    for idx, line in enumerate(nocomment_lines):
        if re.search(r'^#\s*define\s+AZ04_EXEMPT\b', line):
            continue
        if 'AZ04_EXEMPT' in line:
            ok, bad = _valid_az04_exempt_on_line(line)
            if bad or not ok:
                fail_line(idx, 'AZ04_EXEMPT requires a non-empty string literal reason')

    # Classify top-level function spans and check A32-only installer definitions + calls.
    fn_info = _top_level_functions(nocomment_lines, clean_lines, archs)
    fn_arch_by_line = {}
    installer_archs = {}
    for name, hdr_idx, open_idx, end_idx, f_arch in fn_info:
        for li in range(hdr_idx, end_idx + 1):
            fn_arch_by_line[li] = f_arch
        if re.fullmatch(r'(?:install|prepare)_[a-z0-9_]+', name):
            installer_archs.setdefault(name, set()).add(f_arch)

    for name, hdr_idx, open_idx, _end_idx, f_arch in fn_info:
        if f_arch != 'a32' or not re.fullmatch(r'(?:install|prepare)_[a-z0-9_]+', name):
            continue
        if 'a64' in installer_archs[name] or 'common' in installer_archs[name]:
            continue
        if not _has_az04_exempt(nocomment_lines, hdr_idx, open_idx):
            fail_line(open_idx, f'A32-only installer {name} needs AZ04_EXEMPT("reason") or an AZ04 implementation')

    call_re = re.compile(r'\b((?:install|prepare)_[a-z0-9_]+)\s*\(')
    for fn_name, _hdr_idx, open_idx, end_idx, f_arch in fn_info:
        if f_arch != 'common':
            continue
        calls = []
        for li in range(open_idx + 1, end_idx + 1):
            for m in call_re.finditer(clean_lines[li]):
                calls.append((m[1], li))
        non_a32 = {c for c, li in calls if archs[li] in ('a64', 'common')}
        for callee, li in calls:
            if archs[li] == 'a32' and callee not in non_a32 and not _has_az04_exempt(nocomment_lines, li):
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
            for fm in re.finditer(r'\bHF_([A-Z_0-9]+)\b(?!\s*\()', line):
                fid = fm[1]
                if fid in ('ALWAYS', 'COUNT'):
                    fail_line(li, f'HF_{fid} does not belong in az04_exempt_features')
                    continue
                ok, _ = _valid_az04_exempt_on_line(line)
                if not ok:
                    fail_line(li, f'az04_exempt_features entry HF_{fid} needs AZ04_EXEMPT("reason")')
                else:
                    exempt_features.add(fid)

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
    return errors


if __name__ == '__main__':
    path = Path(sys.argv[1]) if len(sys.argv) > 1 else Path(__file__).with_name('engine_ui_hook.c')
    errors = check(path.read_text())
    if errors:
        print('\n'.join(f'{path}:{e}' for e in errors), file=sys.stderr)
        sys.exit(1)
    print('hook feature coverage: PASS')
