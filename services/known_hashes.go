// Code generated from git history of services/compose/*.yml. DO NOT EDIT BY HAND.
// Regenerate with: go run ./services/compose/genhashes (or the shell snippet in PR #426).
//
// KnownComposeHashes maps each citadel-owned service to the set of sha256
// hashes of every compose template content that a prior citadel binary shipped
// for it. Used by internal/composerefresh to safely bootstrap pre-#426 nodes
// that carry no .citadel-managed.json stamp: an on-disk file byte-identical to
// one of these is provably citadel-written (never operator-edited), so it is
// safe to re-materialize; a file matching none is preserved as an operator edit.
package services

// KnownComposeHashes is the historical-template allowlist (see file header).
var KnownComposeHashes = map[string]map[string]bool{
	"ollama": {
		"423edb3435749d6b1c4047b9bfdd2851be733e42e28f189a913701b0aa491e97": true,
		"7e97a822ad30ef8a2cc8e2c20096b24f95a12742bbe533c7bb7de43d37404869": true,
		"fdd34b5e8019bac3330ce25e763135250d1e4ad732109aa10a6e15f2fa16b09d": true,
	},
	"vllm": {
		"07588f4ef245f011396874bcdd7c6aa0c463a356c5f6601618c3b4e213be812a": true,
		"1f8a0b2fc4f49467eec8e9dab860cfae45977634c081f564a796546357f88c94": true,
		"3c19a64a6c7b5f46fac4e621b42cf9505f913759c0fe0bfb10f267b42b3782e9": true,
		"5a6291887c9a317a19b03390a9f9d0cf2cccb2464fb06023927f1ff6095606f6": true,
		"ad24ce421c2358d3214b71efcb57ae2e1d902c44af052e67267e1b5010761f9c": true,
		"af0d1ba61ce68f9ab3ba0811bf0521b6e2af7c868a05051e80288c0f53735258": true,
		"b9d3f74f83089cba9cf7dfc36cdabd18def5f3b17f2a2718ed3125aa34fed593": true,
		"bee8e52e0a1e2cbd47450edb4731f5b63fbc1e5e717e7cc088e5405e6e548afa": true,
		"e24ce8d2eee81f868d4e0d56324726c982d5a7e472eb82fcad54227883051d1e": true,
		"faafb47b3ce0e7755a773177a80597defa7ee59a07d97285f55425ebe7910ef3": true,
		"fbb22432970140a45794760f0775be51d40555aa66aa3a96142b91438776a2ae": true,
	},
	"llamacpp": {
		"03985065ef91d7ea3a512b3dc3029f08e57ca72a9d6ac96e5a6c3d52d1d9f249": true,
		"2fcefa5327f63af9d1d2fbac8e44f92ac44ca595d9fcd1cb27ec42498f084061": true,
		"586347f463e368ff0c9d81f911c935cc8816851be64a20cb14d2b3eb0252251b": true,
		"b0977ce71d1572e45ff818068ebd84cfb4aff57cf51f116cc8a51d3c4d9d65ca": true,
		"b3bbfbe895ee67e0ea60da4151ef3ef40158941e892a94a8b01f05e6d36bb2ea": true,
	},
	"lmstudio": {
		"01a478e9a65d0e895830a1dda476874a9d22c416aa73f48cb46f3ceda3f74d20": true,
		"cd7b791871ae4cd2b9a8a90803aeb630d5a7282445c20e69928d1defbf412946": true,
	},
	"sglang": {
		"2531d9e78484785543f196082ce38e46ef21dd1d6cd380b69665c331bc21acfb": true,
		"2e46554d84a142f549b0481b068b14d2bc65fd02829956ea2288c089b5ad518a": true,
	},
	"extraction": {
		"3f7f02536458773d7f8b22da3e7d4213b943db47c457ce669d9270ff2e0a7260": true,
		"62e82d9afdd1fb109942a71d6f33f9289af21e9391091f5f33103423ee4f854c": true,
		"9f826faa089bf5cba91c9437c1d9feb9e376332f15cdd6fb21539e80ebf3d45e": true,
	},
	"transcribe": {
		"26407f6fde85b0f6af362b25e23b1b3be3342ecc4f47ac38ee43abf96bd719ee": true,
		"e365f728ef81782455a2a18a00a6f7e41d377ae2375d6f6660816cc7f3e0c77d": true,
	},
	"diffusers": {
		"3f2059400959adcc0f55ea8134cb492530ca65d4faf51778a68f35d0d5d9c93a": true,
		"6d57a7a04565fe8954ffe2a2b364813ec2fe700b06f2dfb05567a1a33662d445": true,
		"81bac6f3c78623c1c08761b1fb99718717e84891459cc50d012d02e88cdd32f7": true,
		"83842c7faf86de64d7728e0178690f69a0819354d0f290edbcf3d40ea5f01807": true,
	},
	"bonsai": {
		"124af757f45e93b689bc6a59f08187d48f59c0e62eb2d10c6a006c8ac4d24609": true,
	},
}
