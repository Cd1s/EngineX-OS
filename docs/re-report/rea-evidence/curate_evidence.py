#!/usr/bin/env python3
"""Project complete REA normalized observations into a portable text dossier.

Raw envelopes, Ghidra projects and target bytes remain in the external lab.
Evidence IDs are retained, not regenerated or represented as new REA signatures.
"""
import argparse
import hashlib
import json
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('source', type=Path)
    parser.add_argument('output', type=Path)
    args = parser.parse_args()
    source = args.source.resolve(strict=True)
    if not source.is_dir():
        parser.error('source must be the external REA results directory')
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    fields = ('evidence_id', 'subject', 'predicate_type', 'operation', 'parameters',
              'normalized_result', 'analysis_profile', 'confidence', 'authority',
              'limitations', 'unknowns')
    source_index_path = source/'EVIDENCE-INDEX.json'
    source_index_raw = source_index_path.read_bytes()
    source_index = json.loads(source_index_raw)
    indexed = {source/entry['evidence_file']: entry for entry in source_index}
    paths = sorted(set(source.rglob('*.evidence.json')) |
                   set(source.rglob('*.cli.json')) | set(indexed))
    if not paths:
        parser.error('source contains no REA evidence')
    index = []
    records = []
    by_id = {}
    for path in paths:
        if not path.is_file() or path.stat().st_size == 0:
            continue
        if path.stat().st_size > 64*1024**2:
            raise ValueError('unexpectedly large evidence file: '+str(path))
        raw = path.read_bytes()
        if path in indexed and hashlib.sha256(raw).hexdigest() != indexed[path]['evidence_file_sha256']:
            raise ValueError('source index hash mismatch: '+str(path))
        record = json.loads(raw)
        item = dict(source_file=str(path.relative_to(source)),
                    raw_file_sha256=hashlib.sha256(raw).hexdigest(),
                    record_type='normalized_projection')
        if isinstance(record, dict) and 'evidence_id' in record:
            item.update({k: record[k] for k in fields if k in record})
            evidence_id = record['evidence_id']
            if evidence_id in by_id:
                previous = by_id[evidence_id]
                if any(previous.get(k) != item.get(k) for k in fields):
                    raise ValueError('conflicting Evidence ID: '+evidence_id)
                previous.setdefault('source_aliases', []).append({
                    'source_file': item['source_file'],
                    'raw_file_sha256': item['raw_file_sha256']})
                continue
            by_id[evidence_id] = item
        else:
            # Keep failed CLI attempts distinguishable from successful REA
            # observations; do not give diagnostics a synthetic Evidence ID.
            error = record.get('error', record) if isinstance(record, dict) else record
            summary = ({k: error[k] for k in ('code', 'category', 'message', 'retryable')
                        if k in error} if isinstance(error, dict) else error)
            item.update(record_type='diagnostic', diagnostic=summary,
                        diagnostic_policy='Raw log/details retained in external source file')
        if path in indexed:
            entry = indexed[path]
            item['source_origin'] = {k: entry[k] for k in ('raw', 'raw_sha256', 'source_kind')}
        records.append(item)
    expected_ids = {entry['evidence_id'] for entry in source_index if entry.get('evidence_id')}
    if set(by_id) != expected_ids:
        raise ValueError('portable Evidence IDs differ from the final source index')
    with (output/'observations.jsonl').open('w') as stream:
        for item in records:
            stream.write(json.dumps(item, ensure_ascii=True, separators=(',', ':'))+'\n')
            index.append(dict(line=len(index)+1, source_file=item['source_file'],
                              raw_file_sha256=item['raw_file_sha256'],
                              evidence_id=item.get('evidence_id'),
                              operation=item.get('operation'),
                              record_type=item['record_type'],
                              source_aliases=item.get('source_aliases', [])))
    metadata = dict(source=str(source), count=len(index),
                    unique_evidence_ids=len(by_id),
                    source_index_sha256=hashlib.sha256(source_index_raw).hexdigest(),
                    projection='All successful normalized results, parameters, authority and limitations; typed failure summaries; raw_result and diagnostic details omitted',
                    raw_evidence_policy='Raw JSON envelopes stay in lab; original file SHA-256 and REA Evidence IDs retained',
                    records=index)
    (output/'INDEX.json').write_text(json.dumps(metadata, indent=2)+'\n')
    print(json.dumps(dict(records=len(index), output=str(output))))


if __name__ == '__main__':
    main()
