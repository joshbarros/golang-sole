import { Component, OnInit, inject, signal } from '@angular/core';
import { AuthService } from './core/auth.service';

@Component({
  selector: 'app-security',
  templateUrl: './security.component.html',
})
export class SecurityComponent implements OnInit {
  readonly auth = inject(AuthService);

  readonly enabled = signal(false);
  readonly setup = signal<{ sharedKey: string; otpAuthUri: string } | null>(null);
  readonly recoveryCodes = signal<string[]>([]);
  readonly error = signal('');
  readonly notice = signal('');

  readonly code = signal('');
  readonly disablePassword = signal('');
  readonly currentPassword = signal('');
  readonly newPassword = signal('');

  async ngOnInit(): Promise<void> {
    this.enabled.set((await this.auth.mfaStatus()).enabled);
  }

  field(target: { set(value: string): void }, event: Event): void {
    target.set((event.target as HTMLInputElement).value);
  }

  startSetup = () => this.run(async () => this.setup.set(await this.auth.mfaSetup()));

  turnOn = () =>
    this.run(async () => {
      this.recoveryCodes.set((await this.auth.mfaEnable(this.code().trim())).codes);
      this.enabled.set(true);
      this.setup.set(null);
      this.code.set('');
      this.notice.set('Two-factor authentication is on. Keep the recovery codes somewhere safe.');
    });

  turnOff = () =>
    this.run(async () => {
      await this.auth.mfaDisable(this.disablePassword(), this.code().trim());
    });

  changePassword = () =>
    this.run(async () => {
      await this.auth.changePassword(this.currentPassword(), this.newPassword());
      this.currentPassword.set('');
      this.newPassword.set('');
      this.notice.set('Your password was changed and every other session was ended.');
    });

  signOutEverywhere = () => this.run(() => this.auth.logoutEverywhere());

  private async run(action: () => Promise<unknown>): Promise<void> {
    this.error.set('');
    this.notice.set('');
    try {
      await action();
    } catch (e: unknown) {
      const body = (e as { error?: { title?: string; detail?: string } }).error;
      this.error.set(body?.detail ?? body?.title ?? 'The request failed.');
    }
  }
}
