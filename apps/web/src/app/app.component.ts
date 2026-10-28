import { Component, inject, signal } from '@angular/core';
import { AuthService } from './core/auth.service';
import { LoginComponent } from './login.component';
import { SecurityComponent } from './security.component';
import { WorkspaceComponent } from './workspace.component';

@Component({
  selector: 'app-root',
  imports: [LoginComponent, SecurityComponent, WorkspaceComponent],
  templateUrl: './app.component.html',
})
export class AppComponent {
  readonly auth = inject(AuthService);
  readonly showSecurity = signal(false);

  signOut(): Promise<void> {
    return this.auth.logout();
  }
}
