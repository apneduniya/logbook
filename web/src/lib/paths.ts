/** A path on this site: one leading slash, no scheme, host, backslash or control character. */
export function isLocalPath(p: string): boolean {
	if (!p.startsWith('/') || p.startsWith('//') || p.includes('\\')) return false;
	for (let i = 0; i < p.length; i++) {
		const c = p.charCodeAt(i);
		if (c < 0x20 || c === 0x7f) return false;
	}
	return true;
}
