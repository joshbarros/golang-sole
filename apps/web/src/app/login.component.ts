import { Component, inject, signal } from '@angular/core';
import { AuthService } from './core/auth.service';

type Mode = 'login' | 'register' | 'forgot' | 'reset';

/** The words the API uses when a sign-in needs a verification code. */
const MFA_REQUIRED = 'A verification code is required.';

@Component({
  selector: 'app-login',
  templateUrl: './login.component.html',
})
export class LoginComponent {
  private readonly auth = inject(AuthService);

  readonly mode = signal<Mode>('login');
  readonly email = signal('');
  readonly password = signal('');
  readonly code = signal('');
  readonly needsCode = signal(false);
  readonly error = signal('');
  readonly notice = signal('');
  readonly busy = signal(false);

  private resetLink: { userId: string; token: string } | null = null;

  constructor() {
    // Links in emails land here: ?confirm=<user>&token=… confirms an address, ?reset=<user>&token=… sets a new password.
    const params = new URLSearchParams(window.location.search);
    const token = params.get('token');
    const confirm = params.get('confirm');
    const reset = params.get('reset');
    if (token && confirm) void this.confirm(confirm, token);
    if (token && reset) {
      this.resetLink = { userId: reset, token };
      this.mode.set('reset');
    }
    if (token && (confirm || reset)) window.history.replaceState({}, '', window.location.pathname);
  }

  go(mode: Mode): void {
    this.mode.set(mode);
    this.error.set('');
    this.notice.set('');
    this.needsCode.set(false);
    this.code.set('');
  }

  field(target: { set(value: string): void }, event: Event): void {
    target.set((event.target as HTMLInputElement).value);
  }

  async submit(): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    this.notice.set('');
    try {
      const email = this.email().trim();
      switch (this.mode()) {
        case 'login':
          await this.auth.login(email, this.password(), this.code().trim() || undefined);
          break;
        case 'register': {
          const message = await this.auth.register(email, this.password());
          if (message) {
            this.go('login');
            this.notice.set(message);
          }
          break;
        }
        case 'forgot':
          await this.auth.forgotPassword(email);
          this.go('login');
          this.notice.set('If that address has an account, a reset link is on its way.');
          break;
        case 'reset':
          await this.auth.resetPassword(
            this.resetLink!.userId,
            this.resetLink!.token,
            this.password(),
          );
          this.go('login');
          this.notice.set('Your password was changed. Sign in with the new one.');
          break;
      }
      this.password.set('');
    } catch (e: unknown) {
      this.fail(e);
    } finally {
      this.busy.set(false);
    }
  }

  async resend(): Promise<void> {
    await this.auth.resendConfirmation(this.email().trim());
    this.notice.set('If that address needs confirming, a new link is on its way.');
  }

  private async confirm(userId: string, token: string): Promise<void> {
    try {
      await this.auth.confirmEmail(userId, token);
      this.notice.set('Your email address is confirmed. You can sign in.');
    } catch (e: unknown) {
      this.fail(e);
    }
  }

  private fail(e: unknown): void {
    const body = e as { error?: { title?: string; detail?: string }; status?: number };
    if (body.error?.title === MFA_REQUIRED) {
      this.needsCode.set(true);
      this.notice.set('Enter the code from your authenticator app, or a recovery code.');
      return;
    }
    this.error.set(
      body.error?.detail ??
        body.error?.title ??
        (body.status === 429 ? 'Too many attempts. Try again shortly.' : 'The request failed.'),
    );
  }
}
