// RHZ-104 (FR-RHZ-128): structured Markdown gate bodies become ordered
// DetailItems while non-section prose retains the legacy request surface.
import { describe, expect, it } from 'vitest';
import { validateNodeDetail, type DetailItem } from '@gunnflow/contract';
import { NOTE_TEXT_MAX, NOTE_TRUNCATED_SUFFIX, detailOf, type WireDetailBody } from '../src/details.js';
import { GATE_BODY_LABELS, parseGateBodySections, stripMarkdown } from '../src/gateBody.js';

const allKoreanSections = [
  '## 연결',
  '- [runbook](ops/deploy.md)',
  '## 판단 정보',
  '**41/42** tests pass.',
  '## 문제 상태',
  '배포가 대기 중이다.',
  '## 승인 입력',
  '`approve` or `reject`',
  '## 선택지',
  '1. Approve\n2. Reject',
  '## 되돌림',
  '_Rollback_ within five minutes.',
  '## 권고',
  'Approve.',
].join('\n');

const gateBody = (body: string, extra: Record<string, string> = {}): WireDetailBody => ({
  gates: [{ id: 'gate-1', state: 'approved', body, ...extra }],
});

describe('FR-RHZ-128 structured gate body sections', () => {
  it('S1: all seven Korean sections use the fixed conclusion-first order and suppress duplicate recommendation', () => {
    const detail = detailOf(
      { revision: 12, body: gateBody(allKoreanSections, {
        recommendation: 'existing recommendation',
        decisionReason: 'approved by ops',
        decidedBy: 'operator',
        requestDigest: 'sha256:abc',
      }) },
      'gate-1',
    );

    expect(detail?.items).toEqual([
      { label: '문제 상태', text: '배포가 대기 중이다.' },
      { label: '권고', text: 'Approve.' },
      { label: '승인 입력', text: '`approve` or `reject`' },
      { label: '선택지', text: '1. Approve\n2. Reject' },
      { label: '되돌림', text: 'Rollback within five minutes.' },
      { label: '판단 정보', text: '41/42 tests pass.' },
      { label: '연결', text: 'runbook (ops/deploy.md)' },
      { label: '승인 상태', text: 'unverified' },
      { label: 'decision', text: 'approved by ops' },
      { label: 'decidedBy', text: 'operator' },
      { label: 'digest', text: 'sha256:abc' },
    ]);
    expect(detail?.items.some((item) => item.label === 'request')).toBe(false);
  });

  it('S2: English synonyms with odd casing and collapsed whitespace map to canonical labels', () => {
    const body = [
      '##   pRoBlEm    StAtE   ', 'p',
      '### CONTEXT', 'l',
      '## what   YOU need TO   know', 'd',
      '### oPtIoNs', 'o',
      '## REVERSIBILITY', 'r',
      '## session INPUT', 'input',
      '### recommendation', 'rec',
    ].join('\n');

    expect(parseGateBodySections(body)).toEqual({
      sections: [
        { label: '문제 상태', text: 'p' },
        { label: '권고', text: 'rec' },
        { label: '승인 입력', text: 'input' },
        { label: '선택지', text: 'o' },
        { label: '되돌림', text: 'r' },
        { label: '판단 정보', text: 'd' },
        { label: '연결', text: 'l' },
      ],
      rest: '',
    });
    expect(Object.keys(GATE_BODY_LABELS)).toEqual(['문제 상태', '권고', '승인 입력', '선택지', '되돌림', '판단 정보', '연결']);
    for (const [canonical, synonyms] of Object.entries(GATE_BODY_LABELS)) {
      for (const synonym of synonyms) {
        expect(parseGateBodySections(`## ${synonym}\ntext`).sections, synonym).toEqual([
          { label: canonical, text: 'text' },
        ]);
      }
    }
  });

  it('S3: a partial structured header plus free prose leaves the prose in request', () => {
    const body = [
      'Operator preface.',
      '',
      '## Problem',
      'Deploy is waiting.',
      '## Options',
      'Approve or reject.',
      '## Follow-up',
      'Notify the release channel.',
    ].join('\n');

    expect(detailOf({ revision: 2, body: gateBody(body) }, 'gate-1')?.items).toEqual([
      { label: '문제 상태', text: 'Deploy is waiting.' },
      { label: '선택지', text: 'Approve or reject.' },
      { label: 'request', text: 'Operator preface.\n\n## Follow-up\nNotify the release channel.' },
      { label: '승인 상태', text: 'unverified' },
    ]);
  });

  it('S4: a body without recognised headings is deep-equal to the pre-change gate builder', () => {
    const body = '# Publish?\n\nRelease **v3**.\n\n## Evidence\n41/42 tests pass.';
    const gate = {
      id: 'gate-1',
      state: 'approved',
      body,
      recommendation: 'approve',
      decisionReason: 'looks good',
      decidedBy: 'operator',
      requestDigest: 'sha256:def',
    };
    const legacyItems: DetailItem[] = [
      { label: 'request', text: gate.body },
      { label: 'recommendation', text: gate.recommendation },
      { label: '승인 상태', text: 'unverified' },
      { label: 'decision', text: gate.decisionReason },
      { label: 'decidedBy', text: gate.decidedBy },
      { label: 'digest', text: gate.requestDigest },
    ];

    expect(detailOf({ revision: 3, body: { gates: [gate] } }, 'gate-1')).toEqual({ revision: 3, items: legacyItems });
  });

  it('S5: duplicate canonical sections concatenate their non-empty texts with a blank line', () => {
    expect(parseGateBodySections('## Options\nBlue\n### 선택지\nGreen')).toEqual({
      sections: [{ label: '선택지', text: 'Blue\n\nGreen' }],
      rest: '',
    });
  });

  it('S6: an unknown level-two heading and its text remain in rest', () => {
    expect(parseGateBodySections('## Problem\nKnown\n## Foo\nUnknown\n### Options\nA')).toEqual({
      sections: [
        { label: '문제 상태', text: 'Known' },
        { label: '선택지', text: 'A' },
      ],
      rest: '## Foo\nUnknown',
    });
  });

  it('S7: every produced item passes validateNodeDetail and long section text uses the established cut', () => {
    const longText = 'x'.repeat(NOTE_TEXT_MAX + 1);
    const detail = detailOf({ revision: 4, body: gateBody(`## Decision info\n${longText}`, { recommendation: 'approve' }) }, 'gate-1')!;
    expect(detail.items[0]).toEqual({
      label: '판단 정보',
      text: 'x'.repeat(NOTE_TEXT_MAX) + NOTE_TRUNCATED_SUFFIX,
    });
    expect(validateNodeDetail(detail)).toMatchObject({ ok: true });
    for (const item of detail.items) {
      expect(item.label.length).toBeLessThanOrEqual(256);
      expect(item.text).not.toBe('');
    }
  });

  it('S8: parsing and detail assembly are deterministic', () => {
    expect(parseGateBodySections(allKoreanSections)).toEqual(parseGateBodySections(allKoreanSections));
    const wire = { revision: 8, body: gateBody(allKoreanSections, { recommendation: 'approve' }) };
    expect(detailOf(wire, 'gate-1')).toEqual(detailOf(wire, 'gate-1'));
  });

  it('S9: stripMarkdown removes supported markers, preserves ordered numbers and line breaks, and is idempotent', () => {
    const cases = [
      ['# Heading\n### Subheading ###', 'Heading\nSubheading'],
      ['**bold** / __also bold__ / *emphasis* / _also emphasis_', 'bold / also bold / emphasis / also emphasis'],
      ['`inline` and ```fenced```', '`inline` and ```fenced```'],
      ['- dash\n* star\n+ plus\n1. first\n12. twelfth', 'dash\nstar\nplus\n1. first\n12. twelfth'],
      ['See [runbook](https://example.test/runbook).', 'See runbook (https://example.test/runbook).'],
      ['snake_case and 2026_10_08', 'snake_case and 2026_10_08'],
      ['`## not heading`', '`## not heading`'],
      ['**a *b* c**', 'a b c'],
      ['[[x](y)](z)', 'x (y) (z)'],
      ['a * b * c', 'a * b * c'],
      ['2 * 3 * 4', '2 * 3 * 4'],
      ['__init__ and __main__', '__init__ and __main__'],
      ['(https://x.test/_p_)', '(https://x.test/_p_)'],
      ['`a*b*c`', '`a*b*c`'],
      ['`**kw**`', '`**kw**`'],
      ['`[a](b)`', '`[a](b)`'],
      ['`*x*`', '`*x*`'],
      ['`- x`', '`- x`'],
      ['`# x`', '`# x`'],
      ['x `_y_` z', 'x `_y_` z'],
      ['**a** and `**b**`', 'a and `**b**`'],
      ['\\*literal\\*', '\\*literal\\*'],
    ] as const;

    for (const [markdown, plainText] of cases) {
      expect(stripMarkdown(markdown)).toBe(plainText);
      expect(stripMarkdown(stripMarkdown(markdown))).toBe(plainText);
    }
  });

  it('S9: a recognised section keeps fenced blocks verbatim and is idempotent', () => {
    const body = '## 권고\nrun:\n```sh\n# install\nrm *.tmp *.log\n- x\n```\ndone';
    const recommendation = 'run:\n```sh\n# install\nrm *.tmp *.log\n- x\n```\ndone';
    expect(parseGateBodySections(body)).toEqual({
      sections: [{ label: '권고', text: recommendation }],
      rest: '',
    });
    expect(stripMarkdown(recommendation)).toBe(recommendation);
  });

  it('S9: a whole strong value is unwrapped', () => {
    expect(parseGateBodySections('## 권고\n**Approve.**')).toEqual({
      sections: [{ label: '권고', text: 'Approve.' }],
      rest: '',
    });
  });

  it('S10: fenced headings are ignored and the complete fence remains in rest', () => {
    const body = 'Intro\n```md\n## 권고\nfake\n```\nafter';
    expect(parseGateBodySections(body)).toEqual({ sections: [], rest: body });
  });

  it('S11: markdown-only section text is dropped after stripping and keeps the recommendation field', () => {
    const body = '## 권고\n* * *';
    expect(parseGateBodySections(body)).toEqual({ sections: [], rest: '' });
    expect(detailOf({ revision: 13, body: gateBody(body, { recommendation: 'keep me' }) }, 'gate-1')?.items).toEqual([
      { label: 'request', text: body },
      { label: 'recommendation', text: 'keep me' },
      { label: '승인 상태', text: 'unverified' },
    ]);
  });

  it('S12: decorated headings map to their canonical section', () => {
    expect(parseGateBodySections('## 권고:\ncolon\n## **권고**\nbold')).toEqual({
      sections: [{ label: '권고', text: 'colon\n\nbold' }],
      rest: '',
    });
  });

  it('S13: an unknown deeper heading remains plain text inside a recognised section', () => {
    expect(parseGateBodySections('## 권고\nmain\n### 근거\nwhy')).toEqual({
      sections: [{ label: '권고', text: 'main\n근거\nwhy' }],
      rest: '',
    });
  });

  it('S14: a body containing only empty headings uses the byte-identical legacy path', () => {
    const body = '## 문제 상태\n## 권고\n### Options';
    expect(detailOf({ revision: 14, body: gateBody(body, { recommendation: 'keep me' }) }, 'gate-1')?.items).toEqual([
      { label: 'request', text: body },
      { label: 'recommendation', text: 'keep me' },
      { label: '승인 상태', text: 'unverified' },
    ]);
  });
});
