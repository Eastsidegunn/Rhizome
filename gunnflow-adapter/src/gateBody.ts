/**
 * Canonical gate-body labels and the English headings accepted for each one.
 * Canonical Korean headings are accepted automatically as well.
 */
export const GATE_BODY_LABELS = {
  '문제 상태': ['Problem', 'Problem state'],
  '권고': ['Recommendation'],
  '승인 입력': ['세션 입력 문구', 'Approval input', 'Session input'],
  '선택지': ['Options'],
  '되돌림': ['Rollback', 'Reversibility'],
  '판단 정보': ['Decision info', 'What you need to know'],
  '연결': ['Links', 'Context'],
} as const;

export interface GateBodySection {
  label: string;
  text: string;
}

export interface ParsedGateBody {
  sections: GateBodySection[];
  rest: string;
}

interface ProtectedMarkdown {
  text: string;
  values: string[];
  placeholderPrefix: string;
}

const foldHeading = (heading: string): string => {
  let folded = heading.trim().replace(/[：:]$/u, '').trim();
  const decoration = /^(?:\*\*([^*\n]+)\*\*|__([^_\n]+)__)$/u.exec(folded);
  if (decoration) folded = (decoration[1] ?? decoration[2]!).trim();
  return folded.replace(/\s+/gu, ' ').toLowerCase();
};

/**
 * Convert the small Markdown surface accepted in structured gate sections to
 * plain text. Ordered-list numbers deliberately remain useful text.
 */
export function stripMarkdown(text: string): string {
  const protectedMarkdown: ProtectedMarkdown = {
    text,
    values: [],
    placeholderPrefix: '\uE000MARKDOWN',
  };
  let { placeholderPrefix } = protectedMarkdown;
  while (text.includes(placeholderPrefix)) placeholderPrefix += '_';
  protectedMarkdown.placeholderPrefix = placeholderPrefix;

  const protect = (value: string): string => {
    const placeholder = `${placeholderPrefix}${protectedMarkdown.values.length}\uE001`;
    protectedMarkdown.values.push(value);
    return placeholder;
  };

  // Fenced blocks, including their delimiters, are literal and shielded from
  // the plain-text rules.
  const lines = [...text.matchAll(/.*(?:\r?\n|$)/gu)].filter((match) => match[0] !== '');
  const fencedParts: string[] = [];
  let cursor = 0;
  for (let index = 0; index < lines.length; index += 1) {
    const opening = /^(?: {0,3})(`{3,}|~{3,})[^\r\n]*(?:\r?\n|$)$/u.exec(lines[index]![0]);
    if (!opening) continue;

    const marker = opening[1]![0]!;
    const minimumLength = opening[1]!.length;
    let closingIndex = index + 1;
    for (; closingIndex < lines.length; closingIndex += 1) {
      const closing = /^(?: {0,3})(`{3,}|~{3,})[\t ]*(?:\r?\n|$)$/u.exec(lines[closingIndex]![0]);
      if (closing && closing[1]![0] === marker && closing[1]!.length >= minimumLength) break;
    }
    if (closingIndex === lines.length) continue;

    const openingStart = lines[index]!.index!;
    const closingEnd = lines[closingIndex]!.index! + lines[closingIndex]![0].length;
    fencedParts.push(text.slice(cursor, openingStart), protect(text.slice(openingStart, closingEnd)));
    cursor = closingEnd;
    index = closingIndex;
  }
  fencedParts.push(text.slice(cursor));
  protectedMarkdown.text = fencedParts.join('');

  // Inline code spans, including their delimiters, are also literal.
  let stripped = protectedMarkdown.text.replace(/(`+)([^`\n]*?)\1/gu, (match) => protect(match));

  stripped = stripped
    .replace(/^(?: {0,3})#{1,6}[\t ]+/gmu, '')
    .replace(/[\t ]+#{1,6}[\t ]*$/gmu, '')
    .replace(/^[\t ]*(?:\*(?:[\t ]+\*){2,}|-(?:[\t ]+-){2,}|_(?:[\t ]+_){2,})[\t ]*$/gmu, '')
    .replace(/^(?:[\t ]*)[-*+][\t ]+/gmu, '');

  let previous: string;
  do {
    previous = stripped;
    stripped = stripped
      .replace(/\[([^\]\n]+)\]\(([^)\n]+)\)/gu, '$1 ($2)')
      .replace(/(?<!\\)\*\*(?=\S)([^*\n]*?\S)(?<!\\)\*\*/gu, '$1')
      .replace(
        /(?<![\\\p{L}\p{N}_])__(?=\S)([^_\n]*?\S)(?<!\\)__(?![\p{L}\p{N}_])/gu,
        (match, content: string) => /^[\p{L}\p{N}_]+$/u.test(content) ? match : content,
      )
      .replace(/(?<![\\*\p{L}\p{N}_])\*(?=\S)([^*\n]*?\S)(?<!\\)\*(?![\p{L}\p{N}_*])/gu, '$1')
      .replace(/(?<![\\/\p{L}\p{N}_])_(?=\S)([^_\n]*?\S)(?<!\\)_(?![\p{L}\p{N}_])/gu, '$1');
  } while (stripped !== previous);

  return stripped.replace(
    new RegExp(`${placeholderPrefix}(\\d+)\\uE001`, 'gu'),
    (_match, index: string) => protectedMarkdown.values[Number(index)]!,
  );
}

const canonicalByHeading = new Map<string, string>();
for (const [canonical, synonyms] of Object.entries(GATE_BODY_LABELS)) {
  canonicalByHeading.set(foldHeading(canonical), canonical);
  for (const synonym of synonyms) canonicalByHeading.set(foldHeading(synonym), canonical);
}

interface Heading {
  start: number;
  bodyStart: number;
  level: 2 | 3;
  label?: string;
}

/**
 * Split a gate's Markdown body into recognised decision sections and the
 * remaining prose. Only level-two and level-three ATX headings participate.
 */
export function parseGateBodySections(body: string): ParsedGateBody {
  const headings: Heading[] = [];
  const linePattern = /.*(?:\r?\n|$)/gu;
  const headingPattern = /^(?: {0,3})(#{2,3})(?!#)[\t ]+(.+?)[\t ]*\r?\n?$/u;
  const fencePattern = /^(?: {0,3})(`{3,}|~{3,})/u;
  let fence: { marker: '`' | '~'; length: number } | undefined;

  for (const lineMatch of body.matchAll(linePattern)) {
    const line = lineMatch[0]!;
    if (line === '') continue;
    const fenceMatch = fencePattern.exec(line);
    if (fenceMatch) {
      const run = fenceMatch[1]!;
      if (!fence) {
        fence = { marker: run[0] as '`' | '~', length: run.length };
      } else if (run[0] === fence.marker && run.length >= fence.length) {
        fence = undefined;
      }
      continue;
    }
    if (fence) continue;

    const match = headingPattern.exec(line);
    if (!match) continue;
    const rawTitle = match[2]!.replace(/[\t ]+#+[\t ]*(?:\r?\n)?$/u, '');
    const level = match[1]!.length as 2 | 3;
    const label = canonicalByHeading.get(foldHeading(rawTitle));
    // An unknown deeper heading is content of the current section. Known
    // headings still start a section, including the supported level three.
    if (!label && level === 3) continue;
    headings.push({
      start: lineMatch.index,
      bodyStart: lineMatch.index + line.length,
      level,
      label,
    });
  }

  const sectionByLabel = new Map<string, GateBodySection>();
  const restParts: string[] = [];

  const preamble = body.slice(0, headings[0]?.start ?? body.length).trim();
  if (preamble) restParts.push(preamble);

  headings.forEach((heading, index) => {
    const end = headings[index + 1]?.start ?? body.length;
    if (!heading.label) {
      const unknownSection = body.slice(heading.start, end).trim();
      if (unknownSection) restParts.push(unknownSection);
      return;
    }

    const sectionText = stripMarkdown(body.slice(heading.bodyStart, end)).trim();
    if (!sectionText) return;

    const existing = sectionByLabel.get(heading.label);
    if (existing) {
      existing.text += `\n\n${sectionText}`;
    } else {
      const section = { label: heading.label, text: sectionText };
      sectionByLabel.set(heading.label, section);
    }
  });

  const sections = Object.keys(GATE_BODY_LABELS).flatMap((label) => {
    const section = sectionByLabel.get(label);
    return section ? [section] : [];
  });

  return { sections, rest: restParts.join('\n\n').trim() };
}
