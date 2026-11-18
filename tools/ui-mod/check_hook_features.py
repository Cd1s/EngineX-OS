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
    for call in re.finditer(r'\b(?:try_patch_bytes|patch_bytes|redirect_function)\(\s*([^,\n]+)', source):
        arg = call[1].strip()
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
    if re.search(r'\bmemcpy\(\s*code\b', config):
        fail(start, 'getConfig copy needs HF_CONFIG_COPY')
    # Directly exported interposers (including macro-generated open/stat aliases).
    signature = re.compile(r'^([A-Za-z_][\w *]*?\b(\w+)\([^;{}]*?\)\s*\{)', re.M)
    clean = re.sub(r'"(?:\\.|[^"\\])*"|\x27(?:\\.|[^\x27\\])*\x27|/\*.*?\*/|//[^\n]*', lambda m: re.sub(r'[^\n]', ' ', m[0]), source, flags=re.S)
    for fn in signature.finditer(source):
        if re.search(r'\b(?:static|typedef)\b', fn[1]):
            continue
        previous = source[:fn.start()].rstrip().rstrip('\\').rstrip().splitlines()[-1]
        if not re.fullmatch(r'HF_INTERPOSE\(HF_[A-Z_0-9]+\)', previous):
            fail(fn.start(), f'exported hook {fn[2]} needs HF_INTERPOSE')
    for ctor in re.finditer(r'__attribute__\(\(constructor\)\) static void (\w+)\(', source):
        previous = source[:ctor.start()].rstrip().splitlines()[-1]
        if not re.fullmatch(r'HF_INTERPOSE\(HF_[A-Z_0-9]+\)', previous):
            fail(ctor.start(), f'constructor {ctor[1]} needs HF_INTERPOSE')
    # An added writable-text path must use the feature-tagged primitive, not a new installer.
    fn_header = re.compile(r'^[^\n;{}]*\b(\w+)\([^;{}]*?\)\s*\{', re.M)
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
        if 'mprotect(' in body and fn[1] not in ('rx_code', 'try_patch_bytes', 'apply_engine_edits', 'oa_install'):
            fail(fn.start(), f'{fn[1]} must use a tagged patch primitive')
        if fn[1] == 'oa_install' and re.search(r'mprotect\((?!stub,)', body):
            fail(fn.start(), 'OfflineAnalyzer text writes must use a tagged patch primitive')
    return errors


if __name__ == '__main__':
    path = Path(sys.argv[1]) if len(sys.argv) > 1 else Path(__file__).with_name('engine_ui_hook.c')
    errors = check(path.read_text())
    if errors:
        print('\n'.join(f'{path}:{e}' for e in errors), file=sys.stderr)
        sys.exit(1)
    print('hook feature coverage: PASS')
