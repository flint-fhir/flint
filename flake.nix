{
  description = "Flint — Open Source FHIR Server on a Data Lake";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
      in
      {
        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go_1_26
            buf
            protobuf
            protoc-gen-go
            protoc-gen-go-grpc
            gh
            gopls
            golangci-lint
            lefthook
            go-task
            k3d
            kubectl
            uv
            duckdb
          ];

          shellHook = ''
            echo "🔥 Flint development shell"
            echo "  Go:      $(go version)"
            echo "  Buf:     $(buf --version)"
            echo "  Protoc:  $(protoc --version)"
            lefthook install >/dev/null 2>&1 || true
          '';
        };
      });
}
