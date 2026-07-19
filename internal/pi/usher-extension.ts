import { stat } from "node:fs/promises";
import { basename, extname, resolve } from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";

const params = Type.Object({
	file_path: Type.String({ description: "Image path, absolute or relative to the session working directory" }),
});

const supported = new Set([".png", ".jpg", ".jpeg", ".gif", ".webp"]);

export default function usherExtension(pi: ExtensionAPI) {
	pi.registerTool({
		name: "show_image",
		label: "Show image",
		description: "Display a local image inline to the user. Use this after creating or editing an image the user should see.",
		parameters: params,
		async execute(_toolCallId, input, _signal, _onUpdate, ctx) {
			const raw = input.file_path.trim();
			if (!raw) throw new Error("show_image: file_path is required");
			const path = resolve(ctx.cwd, raw);
			if (!supported.has(extname(path).toLowerCase())) {
				throw new Error("show_image: supported types are .png, .jpg, .jpeg, .gif, and .webp");
			}
			const info = await stat(path);
			if (!info.isFile()) throw new Error(`show_image: not a regular file: ${raw}`);
			return {
				content: [{ type: "text", text: JSON.stringify({ message: `Showing ${basename(path)} to the user.` }) }],
				details: { file_path: raw },
			};
		},
	});
}
