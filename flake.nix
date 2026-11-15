{
  description = "A small giveaway marketplace for friends: photos, lotteries, magic-link sign-in";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs: rec {
        marketplace = pkgs.buildGoModule {
          pname = "marketplace";
          version = "0.1.0";
          src = self;
          vendorHash = "sha256-n+WU7qcF6NauBw9DFU4VG1fnDQ387+nibJ/uzApqehk=";
          ldflags = [ "-s" "-w" ];
          meta = {
            description = "Giveaway marketplace for friends";
            mainProgram = "marketplace";
          };
        };
        default = marketplace;
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell { packages = [ pkgs.go pkgs.gopls ]; };
      });

      nixosModules.default = { config, lib, pkgs, ... }:
        let
          cfg = config.services.marketplace;
          inherit (lib) mkOption types;
          credentials =
            lib.optional (cfg.mail.resendApiKeyFile != null) "resend_api_key:${cfg.mail.resendApiKeyFile}"
            ++ lib.optional (cfg.mail.smtp.passwordFile != null) "smtp_password:${cfg.mail.smtp.passwordFile}";
        in
        {
          options.services.marketplace = {
            enable = lib.mkEnableOption "the giveaway marketplace";

            address = mkOption {
              type = types.str;
              default = "127.0.0.1";
              description = "Address to bind.";
            };

            port = mkOption {
              type = types.port;
              default = 8080;
              description = "Port to listen on.";
            };

            baseUrl = mkOption {
              type = types.str;
              example = "https://stuff.example.com";
              description = "Public URL, used in emailed links. An https:// URL also marks cookies Secure.";
            };

            siteTitle = mkOption {
              type = types.str;
              default = "Free Stuff";
              example = "Sam's Move-Out Giveaway";
              description = "Name shown in the header, page titles and emails.";
            };

            adminEmails = mkOption {
              type = types.listOf types.str;
              example = [ "me@example.com" ];
              description = "Emails that can post and manage items. They can always sign in.";
            };

            newItemsDelay = mkOption {
              type = types.str;
              default = "10m";
              description = ''
                New-item emails go out once nothing new has been posted for this
                long (Go duration), so posting several items sends one email.
              '';
            };

            mail = {
              from = mkOption {
                type = types.nullOr types.str;
                default = null;
                example = "Free Stuff <stuff@mail.example.com>";
                description = "From address. Required when a mail transport is configured.";
              };
              resendApiKeyFile = mkOption {
                type = types.nullOr types.path;
                default = null;
                description = "File holding a Resend API key. Takes precedence over SMTP.";
              };
              smtp = {
                host = mkOption { type = types.nullOr types.str; default = null; };
                port = mkOption { type = types.port; default = 587; description = "587 uses STARTTLS, 465 implicit TLS."; };
                username = mkOption { type = types.nullOr types.str; default = null; };
                passwordFile = mkOption { type = types.nullOr types.path; default = null; };
              };
            };

            package = mkOption {
              type = types.package;
              default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
              description = "marketplace package to run.";
            };
          };

          config = lib.mkIf cfg.enable {
            assertions = [
              {
                assertion = cfg.adminEmails != [ ];
                message = "services.marketplace.adminEmails must list at least one admin.";
              }
              {
                assertion = (cfg.mail.resendApiKeyFile != null || cfg.mail.smtp.host != null) -> cfg.mail.from != null;
                message = "services.marketplace.mail.from is required when a mail transport is configured.";
              }
            ];

            systemd.services.marketplace = {
              description = "Giveaway marketplace";
              wantedBy = [ "multi-user.target" ];
              after = [ "network-online.target" ];
              wants = [ "network-online.target" ];

              environment = {
                ADDR = "${cfg.address}:${toString cfg.port}";
                BASE_URL = cfg.baseUrl;
                DATA_DIR = "/var/lib/marketplace";
                SITE_TITLE = cfg.siteTitle;
                ADMIN_EMAILS = lib.concatStringsSep "," cfg.adminEmails;
                NEW_ITEMS_DELAY = cfg.newItemsDelay;
              }
              // lib.optionalAttrs (cfg.mail.from != null) { MAIL_FROM = cfg.mail.from; }
              // lib.optionalAttrs (cfg.mail.resendApiKeyFile != null) { RESEND_API_KEY_FILE = "%d/resend_api_key"; }
              // lib.optionalAttrs (cfg.mail.smtp.host != null) {
                SMTP_HOST = cfg.mail.smtp.host;
                SMTP_PORT = toString cfg.mail.smtp.port;
              }
              // lib.optionalAttrs (cfg.mail.smtp.username != null) { SMTP_USERNAME = cfg.mail.smtp.username; }
              // lib.optionalAttrs (cfg.mail.smtp.passwordFile != null) { SMTP_PASSWORD_FILE = "%d/smtp_password"; };

              serviceConfig = {
                ExecStart = lib.getExe cfg.package;
                DynamicUser = true;
                StateDirectory = "marketplace";
                # systemd reads these as root and hands the service private
                # copies, so secret files can stay root-only.
                LoadCredential = credentials;

                # If the bind address (e.g. a Tailscale IP) isn't up yet,
                # keep retrying rather than staying dead.
                Restart = "always";
                RestartSec = 5;

                NoNewPrivileges = true;
                PrivateTmp = true;
                PrivateDevices = true;
                ProtectSystem = "strict";
                ProtectHome = true;
                ProtectKernelTunables = true;
                ProtectKernelModules = true;
                ProtectControlGroups = true;
                RestrictNamespaces = true;
                LockPersonality = true;
                RestrictAddressFamilies = [ "AF_INET" "AF_INET6" "AF_UNIX" ];
                CapabilityBoundingSet = "";
                SystemCallFilter = [ "@system-service" ];
                UMask = "0077";
              };
            };
          };
        };
    };
}
