{ pkgs, version, steward-main }:

pkgs.buildNpmPackage (finalAttrs: {
  pname = "steward-pi-runtime";
  inherit version;
  src = ../.;
  nodejs = pkgs.nodejs_24;
  npmDepsHash = "sha256-MKroxqt7+8a9Uo0exMiCBG9pOlR/Wq/udmm35Yfxqa0=";
  npmDepsFetcherVersion = 2;
  npmFlags = [ "--ignore-scripts" ];
  dontBuild = true;
  dontNpmBuild = true;

  nativeBuildInputs = [ pkgs.makeWrapper ];

  installPhase = ''
    runHook preInstall
    ${pkgs.nodejs_24}/bin/node runtime/prepare-pi-runtime.mjs
    rm node_modules/@earendil-works/pi-coding-agent/node_modules/.bin/pi-ai
    mkdir -p $out/lib/steward $out/bin
    cp -R package.json runtime node_modules $out/lib/steward/
    chmod -R u+w $out/lib/steward
    makeWrapper ${pkgs.nodejs_24}/bin/node $out/bin/steward-pi-helper \
      --add-flags $out/lib/steward/runtime/cli.mjs \
      --prefix PATH : ${pkgs.lib.makeBinPath [ steward-main pkgs.ripgrep pkgs.fd ]}
    makeWrapper ${pkgs.nodejs_24}/bin/node $out/bin/pi \
      --add-flags $out/lib/steward/node_modules/@earendil-works/pi-coding-agent/dist/cli.js \
      --prefix PATH : "$out/bin:${pkgs.lib.makeBinPath [ steward-main pkgs.ripgrep pkgs.fd ]}"
    runHook postInstall
  '';

  passthru = {
    extensionRoot = "${finalAttrs.finalPackage}/lib/steward";
    nodeModules = "${finalAttrs.finalPackage}/lib/steward/node_modules";
  };

  meta = with pkgs.lib; {
    description = "Steward's pinned Pi runtime and helper";
    mainProgram = "pi";
    platforms = platforms.unix;
  };
})
