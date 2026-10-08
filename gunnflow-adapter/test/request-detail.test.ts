// FR-RHZ-160: structured request detail order, raw command bytes and max size.
import { describe, expect, it } from 'vitest';
import { validateNodeDetail } from '@gunnflow/contract';
import { detailOf } from '../src/details.js';

describe('request detail FR-RHZ-160', () => {
  it('uses the fixed labels and preserves repeated command bytes exactly', () => {
    const commands = ['printf "  a  b  "', `# raw\n${'x'.repeat(1994)}`];
    const detail = detailOf({ revision: 4, body: { requests: [{
      id: 'r-1', state: 'done', why: 'why', where: 'where', commands,
      after: 'verify', rollback: 'undo', memo: 'worked', closedBy: 'operator',
      closedAt: '2026-10-08T01:02:03Z',
    }] } }, 'r-1')!;
    expect(detail.items.map((item) => item.label)).toEqual([
      '왜 필요한가', '어디서', '명령', '명령', '끝나면', '되돌림', '결과 메모', '닫은 이', '닫은 시각',
    ]);
    expect(detail.items.filter((item) => item.label === '명령').map((item) => item.text)).toEqual(commands);
    expect(detail.items[3]!.text).toHaveLength(2000);
    expect(detail.items[2]!.text?.endsWith('\n')).toBe(false);
    expect(validateNodeDetail(detail).ok).toBe(true);
  });

  it('emits 39 valid items at the maximum command count', () => {
    const detail = detailOf({ revision: 5, body: { requests: [{
      id: 'r-max', state: 'unable', why: 'why', where: 'where',
      commands: Array.from({ length: 32 }, (_, i) => `command-${i}`),
      after: 'verify', rollback: 'undo', reason: 'cannot', closedBy: 'operator',
      closedAt: '2026-10-08T01:02:03Z',
    }] } }, 'r-max')!;
    expect(detail.items).toHaveLength(39);
    expect(detail.items.slice(-3).map((item) => item.label)).toEqual(['사유', '닫은 이', '닫은 시각']);
    expect(validateNodeDetail(detail).ok).toBe(true);
  });
});
